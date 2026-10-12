// 渲染 Session 的辅助：正在生成的草稿、ask_user 提问、卡片（docs/design/m3-duplex-channel.md §4、§7），
// 以及进行中的委托里等待用户的提问与审批（docs/design/m5-multi-agent.md §4）。

import type { Client } from "./client.js";
import type { Card, Event, ToolCall } from "./gen/yanshi/v1/event_pb.js";
import type { LiveDelta } from "./gen/yanshi/v1/node_pb.js";

/**
 * Draft 维护一个 Session 正在生成、尚未提交的助手消息，规则见设计 §4：
 *   - snapshot 增量替换草稿（生成中途订阅时先收到它）；普通增量追加；
 *   - end 增量，或 seq 大于草稿 afterSeq 的 AssistantMessage 到达时，丢弃草稿：以提交的内容为准；
 *   - 草稿所属的 Run 结束（completed、failed、interrupted）时丢弃草稿，之后该 Run 迟到的增量不再建立草稿。
 *     断线期间生成被丢弃（end 没有收到）、Run 随后结束时，重连后补发的只有终态事件，没有它草稿会一直留着。
 *
 * 把订阅收到的增量与事件依次交给 onDelta 与 onEvent，渲染 text 即可。
 */
export class Draft {
  /** 草稿全文；没有正在生成的消息时为空。 */
  text = "";
  /** 正在生成的思考过程（ADR-0034），与正文分开显示；提交后以 AssistantMessage.reasoning 为准。 */
  reasoning = "";
  /** 草稿所属的 Run；没有正在生成的消息时为空。 */
  runId = "";
  #attempt = 0;
  #afterSeq = -1n;
  // 已提交的最新 AssistantMessage 的 seq：after_seq 小于它的增量属于已提交的生成，是迟到的残留。
  #committed = 0n;
  // 最近结束的 Run：它们迟到的增量忽略。只需记住最近几个：迟到的增量在 Run 结束后很快到达。
  readonly #ended = new Set<string>();

  get active(): boolean {
    return this.runId !== "";
  }

  /** 应用一条增量；返回草稿是否变化。 */
  onDelta(d: LiveDelta): boolean {
    if (d.afterSeq < this.#committed || this.#ended.has(d.runId)) return false;
    const same = d.runId === this.runId && d.attempt === this.#attempt && d.afterSeq === this.#afterSeq;
    if (d.end) {
      if (!same) return false;
      this.#clear();
      return true;
    }
    if (d.snapshot || !same) {
      this.runId = d.runId;
      this.#attempt = d.attempt;
      this.#afterSeq = d.afterSeq;
      this.text = d.text;
      this.reasoning = d.reasoning;
      return true;
    }
    this.text += d.text;
    this.reasoning += d.reasoning;
    return d.text !== "" || d.reasoning !== "";
  }

  /** 应用一条已提交的事件；返回草稿是否变化。 */
  onEvent(e: Event): boolean {
    const p = e.payload;
    if (p.case === "runCompleted" || p.case === "runFailed" || p.case === "runInterrupted") {
      this.#ended.add(p.value.runId);
      if (this.#ended.size > 64) this.#ended.delete(this.#ended.values().next().value!);
      if (this.runId !== p.value.runId) return false;
      this.#clear();
      return true;
    }
    if (p.case !== "assistantMessage") return false;
    if (e.seq > this.#committed) this.#committed = e.seq;
    if (!this.active || e.seq <= this.#afterSeq) return false;
    this.#clear();
    return true;
  }

  #clear(): void {
    this.text = "";
    this.reasoning = "";
    this.runId = "";
    this.#attempt = 0;
    this.#afterSeq = -1n;
  }
}

/** ask_user 的能力名。 */
export const ASK_USER = "ask_user";

/** ask_user 调用派发时 ToolCallStarted 的 node_id：路由到用户本人（ADR-0025）。 */
export const USER_NODE = "@user";

export interface QuestionOption {
  id: string;
  label: string;
  description?: string;
}

export interface QuestionField {
  name: string;
  label: string;
  /** "text"（默认）、"number"、"date"（YYYY-MM-DD）或 "time"（HH:MM）。 */
  type?: "text" | "number" | "date" | "time";
  required?: boolean;
}

/** Agent 经 ask_user 提出的问题（ADR-0025）。 */
export interface Question {
  callId: string;
  question: string;
  options: QuestionOption[];
  multiple: boolean;
  fields: QuestionField[];
}

/**
 * 从 AssistantMessage 的工具调用中读出 ask_user 提问；不是提问或参数无法解析时返回 undefined。
 * 参数不合法的提问不会到达用户（Worker 直接把错误交还模型），所以这里不再校验。
 */
export function questionOf(call: ToolCall): Question | undefined {
  if (call.capability !== ASK_USER) return undefined;
  try {
    const a = JSON.parse(call.argumentsJson) as Partial<Omit<Question, "callId">>;
    return {
      callId: call.callId,
      question: a.question ?? "",
      options: a.options ?? [],
      multiple: a.multiple ?? false,
      fields: a.fields ?? [],
    };
  } catch {
    return undefined;
  }
}

/**
 * Asked 记下 AssistantMessage 中的提问，派发（ToolCallStarted{node_id: "@user"}）之后才算待回答。
 *
 * Worker 按顺序一次推进一个调用：模型同一轮返回"委托 + ask_user"时，提问要等委托结束才派发。在那之前它不能回答
 * （服务端只接受已派发的提问），输入框的文字也不该因为它而不去回答子 Agent 的提问。
 */
class Asked {
  readonly asked = new Map<string, { question: Question; runId: string }>();
  readonly open = new Map<string, { question: Question; runId: string }>();

  /** 应用一条事件；返回待回答的提问是否变化。 */
  apply(e: Event): boolean {
    const p = e.payload;
    switch (p.case) {
      case "assistantMessage":
        for (const c of p.value.toolCalls) {
          const q = questionOf(c);
          if (q) this.asked.set(q.callId, { question: q, runId: p.value.runId });
        }
        return false;
      case "toolCallStarted": {
        const q = this.asked.get(p.value.callId);
        if (!q || p.value.nodeId !== USER_NODE) return false;
        this.asked.delete(p.value.callId);
        this.open.set(p.value.callId, q);
        return true;
      }
      case "toolResult":
        this.asked.delete(p.value.callId);
        return this.open.delete(p.value.callId);
      case "runCompleted":
      case "runFailed":
      case "runInterrupted": {
        const runId = p.value.runId;
        for (const [id, q] of this.asked) if (q.runId === runId) this.asked.delete(id);
        let changed = false;
        for (const [id, q] of this.open) if (q.runId === runId) changed = this.open.delete(id) || changed;
        return changed;
      }
      default:
        return false;
    }
  }

  clear(): boolean {
    const changed = this.open.size > 0;
    this.asked.clear();
    this.open.clear();
    return changed;
  }
}

/**
 * PendingQuestions 跟踪一个 Session 中等待回答的提问：提问随 AssistantMessage 出现，该调用派发给用户
 * （ToolCallStarted 的 node_id 为 "@user"）后才算待回答，在出现 ToolResult（任一设备回答、超时）或 Run 结束时收起。
 */
export class PendingQuestions {
  readonly #q = new Asked();

  get questions(): Question[] {
    return [...this.#q.open.values()].map((x) => x.question);
  }

  /** 应用一条已提交的事件；返回列表是否变化。 */
  onEvent(e: Event): boolean {
    return this.#q.apply(e);
  }
}

/** 卡片的一个版本：卡片本身与收到它的事件序号（提交动作时带上，见 YanshiClient.submitCardAction）。 */
export interface CardVersion {
  card: Card;
  seq: bigint;
}

/**
 * Cards 跟踪一个 Session 中展示过的卡片（docs/design/cards.md）：卡片随 show_card 的结果出现，同一 card_id
 * 再次展示时替换前一版，只保留最新版。cards 按卡片第一次出现的顺序排列。
 */
export class Cards {
  readonly #byId = new Map<string, CardVersion>();

  get cards(): CardVersion[] {
    return [...this.#byId.values()];
  }

  get(cardId: string): CardVersion | undefined {
    return this.#byId.get(cardId);
  }

  /** 应用一条已提交的事件；返回是否有卡片出现或被替换。 */
  onEvent(e: Event): boolean {
    const c = cardOf(e);
    if (!c) return false;
    this.#byId.set(c.cardId, { card: c, seq: e.seq });
    return true;
  }
}

/** 返回事件中的卡片（show_card 的结果）；没有时返回 undefined。 */
export function cardOf(e: Event): Card | undefined {
  if (e.payload.case !== "toolResult") return undefined;
  for (const b of e.payload.value.content) {
    if (b.kind.case === "card") return b.kind.value;
  }
  return undefined;
}

/** 委托调用的 ToolCallStarted.node_id 前缀：其后是子 Session 的 ID（docs/design/m5-multi-agent.md §4）。 */
export const AGENT_NODE_PREFIX = "@agent/";

interface PendingBase {
  /** 回答或审批发往的 Session：顶层 Session，或进行中的委托派生的子 Session。 */
  sessionId: string;
  callId: string;
  runId: string;
  /** 提出它的子 Agent（AgentDef 名，去掉 agent__ 前缀）；顶层 Session 自己的为空。 */
  agent: string;
  /** 经由顶层 Session 的哪个直接委托的子 Session（孙 Session 里的也归到它）；顶层 Session 自己的为空。 */
  via: string;
}

/** 等待用户回答的 ask_user 提问。 */
export interface PendingQuestion extends PendingBase {
  kind: "question";
  question: Question;
}

/** 等待用户审批的调用。 */
export interface PendingApproval extends PendingBase {
  kind: "approval";
  /** 给用户看的说明（ApprovalRequested.summary）。 */
  summary: string;
  /** 要执行的能力（模型调用的工具名）。 */
  capability: string;
}

export type PendingItem = PendingQuestion | PendingApproval;

export interface PendingTrackerOptions {
  /** 待处理项变化时调用，包括子 Session 中的变化（它们不经应用的订阅）。 */
  onChange?: (items: PendingItem[]) => void;
  /** 子 Session 的订阅以 not_found 或 error 结束后重订的退避：从 retryMinMs（默认 200）起翻倍，至多 retryMaxMs（默认 5000）。 */
  retryMinMs?: number;
  retryMaxMs?: number;
}

// 子 Session 的订阅以这些原因结束、而委托仍在进行时，退避后重订。Worker 先提交 ToolCallStarted、再创建子 Session，
// 客户端发现委托立即订阅时，子 Session 可能还不存在（not_found）；error 是网关一侧的暂时故障。deleted 不重订。
const RETRY_REASONS = new Set(["not_found", "error"]);

// 被跟踪的一个 Session：顶层 Session，或进行中的委托派生的子 Session。
class Tracked {
  // 本 Session 的调用：call_id → 能力与所属 Run。审批的能力名、委托的子 Agent 名从这里取。
  readonly calls = new Map<string, { capability: string; runId: string }>();
  readonly questions = new Asked();
  readonly approvals = new Map<string, { summary: string; capability: string; runId: string }>();
  // 进行中的委托：call_id → 子 Session。
  readonly delegations = new Map<string, { runId: string; child: Tracked }>();
  off: (() => void) | undefined;
  // 子 Session 已收到的最后一个 seq：重订时从这里续传，已有的待处理项不必重建。
  after = 0n;
  // 重订的退避（毫秒）与定时器。
  backoff = 0;
  timer: ReturnType<typeof setTimeout> | undefined;

  constructor(
    readonly sessionId: string,
    readonly agent: string,
    readonly via: string,
  ) {}
}

/**
 * PendingTracker 跟踪一个顶层 Session 及其进行中的委托（递归）里等待用户的提问与审批。
 *
 * 委托出去的子 Run 的 ask_user 与审批路由到用户本人，却写在子 Session 的日志里：客户端只看顶层 Session 时看不到，
 * 父 Run 就一直等着委托直到超时（docs/design/m5-multi-agent.md §4）。所以：
 *   - 顶层 Session 的事件由应用喂给 onEvent（和 Draft、PendingQuestions 一样，从 seq 0 开始）；
 *   - 被跟踪的 Session 出现委托（ToolCallStarted 的 node_id 为 "@agent/<子 Session>"）时，经 Client 订阅子 Session
 *     的完整历史；该委托调用出现 ToolResult、或它所在的 Run 结束时，取消订阅并收起子 Session（及其后代）的待处理项；
 *   - 子 Session 的订阅以 not_found 或 error 结束而委托仍在进行时（子 Session 可能还没创建），退避后从已收到的位置重订；
 *     以其他原因（deleted 等）结束时收起它的待处理项，不再订阅；
 *   - 提问随 AssistantMessage 出现、派发给用户（ToolCallStarted 的 node_id 为 "@user"）后才算待回答，在该调用出现
 *     ToolResult 或 Run 结束时收起；审批随 ApprovalRequested 出现，在 ApprovalDecided、该调用的 ToolResult 或 Run 结束时收起。
 *
 * 回答与审批发往待处理项的 sessionId。辅助类对子 Session 的订阅与应用自己的订阅并存：应用另外打开子 Session 不受影响。
 * 不再需要时调用 close。
 */
export class PendingTracker {
  readonly #client: Client;
  readonly #root: Tracked;
  readonly #onChange: ((items: PendingItem[]) => void) | undefined;
  readonly #retryMin: number;
  readonly #retryMax: number;
  // 树中的全部 Session：防止委托链成环时重复订阅。
  readonly #tracked = new Set<string>();
  #closed = false;

  constructor(client: Client, sessionId: string, opts: PendingTrackerOptions = {}) {
    this.#client = client;
    this.#root = new Tracked(sessionId, "", "");
    this.#tracked.add(sessionId);
    this.#onChange = opts.onChange;
    this.#retryMin = opts.retryMinMs ?? 200;
    this.#retryMax = opts.retryMaxMs ?? 5000;
  }

  get sessionId(): string {
    return this.#root.sessionId;
  }

  /** 全部待处理项，按委托树深度优先排列：每个 Session 先审批、后提问，然后是它的子 Session。 */
  get items(): PendingItem[] {
    const out: PendingItem[] = [];
    const walk = (t: Tracked) => {
      const base = { sessionId: t.sessionId, agent: t.agent, via: t.via };
      for (const [callId, a] of t.approvals) out.push({ kind: "approval", ...base, callId, ...a });
      for (const [callId, q] of t.questions.open) out.push({ kind: "question", ...base, callId, ...q });
      for (const d of t.delegations.values()) walk(d.child);
    };
    walk(this.#root);
    return out;
  }

  /**
   * 输入框里的文字应作为哪个提问的回答（以 Client.answer 的 text 发往它的 sessionId）：顶层 Session 自己没有待回答的提问、
   * 整棵树里恰好一个待回答的提问、且它在子 Session 中时返回它，否则返回 undefined，文字照旧提交给顶层 Session
   * （顶层自己的提问由服务端把文字当作回答）。尚未派发的提问不算待回答。
   *
   * 不由服务端沿委托链把父 Session 的输入转给子 Session：一次输入跨两个 Session 不能在一个事务中提交（ADR-0028），
   * 父日志里也没有可以去重的输入。回答按 call_id 去重：别的设备已经回答时得到 conflict，此时应保留文字、提示用户，
   * 不要自动改投顶层 Session。
   */
  typedAnswerTarget(): PendingQuestion | undefined {
    if (this.#root.questions.open.size > 0) return undefined;
    const questions = this.items.filter((i): i is PendingQuestion => i.kind === "question");
    return questions.length === 1 ? questions[0] : undefined;
  }

  /** 应用顶层 Session 的一条已提交事件；返回待处理项是否变化（变化时也调用 onChange）。其他 Session 的事件忽略。 */
  onEvent(e: Event): boolean {
    if (this.#closed || e.sessionId !== this.#root.sessionId) return false;
    return this.#update(this.#root, e);
  }

  /** 取消全部子 Session 的订阅与重订；之后不再回调。 */
  close(): void {
    if (this.#closed) return;
    this.#closed = true;
    for (const d of this.#root.delegations.values()) this.#drop(d.child);
    this.#root.delegations.clear();
    this.#root.questions.clear();
    this.#root.approvals.clear();
  }

  #update(t: Tracked, e: Event): boolean {
    const changed = this.#apply(t, e);
    if (changed) this.#onChange?.(this.items);
    return changed;
  }

  // #apply 把一条事件应用到被跟踪的 Session；返回待处理项是否变化。
  #apply(t: Tracked, e: Event): boolean {
    const p = e.payload;
    let changed = t.questions.apply(e);
    switch (p.case) {
      case "assistantMessage": {
        for (const c of p.value.toolCalls) t.calls.set(c.callId, { capability: c.capability, runId: p.value.runId });
        return changed;
      }
      case "approvalRequested": {
        const { callId, runId, summary } = p.value;
        t.approvals.set(callId, { summary, capability: t.calls.get(callId)?.capability ?? "", runId });
        return true;
      }
      case "approvalDecided":
        return t.approvals.delete(p.value.callId);
      case "toolCallStarted": {
        const { callId, runId, nodeId } = p.value;
        if (!nodeId.startsWith(AGENT_NODE_PREFIX) || t.delegations.has(callId)) return changed;
        const sid = nodeId.slice(AGENT_NODE_PREFIX.length);
        if (this.#tracked.has(sid)) return changed;
        const agent = (t.calls.get(callId)?.capability ?? "").replace(/^agent__/, "");
        const child = new Tracked(sid, agent, t.via || sid);
        t.delegations.set(callId, { runId, child });
        this.#tracked.add(sid);
        this.#observe(t, callId, child);
        return changed;
      }
      case "toolResult": {
        const id = p.value.callId;
        changed = t.approvals.delete(id) || changed;
        const d = t.delegations.get(id);
        if (d) {
          t.delegations.delete(id);
          changed = this.#drop(d.child) || changed;
        }
        return changed;
      }
      case "runCompleted":
      case "runFailed":
      case "runInterrupted": {
        const runId = p.value.runId;
        for (const [id, a] of t.approvals) if (a.runId === runId) changed = t.approvals.delete(id) || changed;
        for (const [id, d] of t.delegations) {
          if (d.runId !== runId) continue;
          t.delegations.delete(id);
          changed = this.#drop(d.child) || changed;
        }
        for (const [id, c] of t.calls) if (c.runId === runId) t.calls.delete(id);
        return changed;
      }
      default:
        return changed;
    }
  }

  // #observe 订阅子 Session：第一次从头（它的提问与审批可能早于这次订阅），重订时从已收到的位置续传。
  #observe(parent: Tracked, callId: string, child: Tracked): void {
    const live = () => !this.#closed && parent.delegations.get(callId)?.child === child;
    child.off = this.#client._observe(child.sessionId, child.after, {
      onEvent: (ce) => {
        if (!live()) return;
        child.after = ce.seq;
        child.backoff = 0;
        this.#update(child, ce);
      },
      onEnded: (reason) => {
        if (!live()) return;
        child.off = undefined;
        if (RETRY_REASONS.has(reason)) {
          // 委托仍在进行：退避后重订，已有的待处理项保留。
          child.backoff = Math.min(child.backoff ? child.backoff * 2 : this.#retryMin, this.#retryMax);
          child.timer = setTimeout(() => {
            child.timer = undefined;
            if (live()) this.#observe(parent, callId, child);
          }, child.backoff);
          return;
        }
        // 订阅被网关结束（子 Session 被删除等）：它的待处理项已无法处理，收起；委托本身仍等父 Session 的结果。
        if (this.#clear(child)) this.#onChange?.(this.items);
      },
    });
  }

  // #drop 取消一个子 Session（及其后代）的订阅与重订；返回是否收起了待处理项。
  #drop(t: Tracked): boolean {
    t.off?.();
    t.off = undefined;
    clearTimeout(t.timer);
    t.timer = undefined;
    this.#tracked.delete(t.sessionId);
    return this.#clear(t);
  }

  // #clear 收起一个 Session 的待处理项，并放下它的全部子 Session；返回是否收起了待处理项。
  #clear(t: Tracked): boolean {
    let changed = t.questions.clear() || t.approvals.size > 0;
    t.approvals.clear();
    for (const d of t.delegations.values()) changed = this.#drop(d.child) || changed;
    t.delegations.clear();
    return changed;
  }
}
