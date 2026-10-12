// 会话客户端（docs/design/m3-duplex-channel.md）：经一条 WebSocket 订阅 Session 的事件流，
// 并提交输入、中断、审批、回答提问。网页不提供 Capability，连接只作会话客户端（Hello.client_only，ADR-0024）。
//
// 语义与 Go SDK（sdk/nodesdk）一致：
//   - 断线后以指数退避重连，并按各订阅最后收到的 seq 续订：事件不漏、不重；
//   - 提交输入带输入 ID，连接在响应之前断开时，重连后以同一 ID 自动重试：输入恰好生效一次；
//   - 回调按到达顺序串行调用。

import { create, fromBinary, toBinary, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampDate } from "@bufbuild/protobuf/wkt";
import type { ContentBlock, Event } from "./gen/yanshi/v1/event_pb.js";
import { ContentBlockSchema } from "./gen/yanshi/v1/event_pb.js";
import {
  CallAction,
  GatewayMessageSchema,
  NodeMessageSchema,
  type CallEvent,
  type CallReady,
  type CallTask,
  type CallText,
  type ClientRequestSchema,
  type ClientResponse,
  type GatewayMessage,
  type LiveDelta,
  type NodeMessage,
  type Presence,
} from "./gen/yanshi/v1/node_pb.js";

export const VERSION = "0.1.0";

/** 运行环境提供的 WebSocket 的最小子集（浏览器、Node.js 22+ 的全局 WebSocket 都满足）。 */
export interface WebSocketLike {
  binaryType: string;
  send(data: Uint8Array): void;
  close(code?: number, reason?: string): void;
  onopen: ((ev: unknown) => void) | null;
  onmessage: ((ev: { data: unknown }) => void) | null;
  onclose: ((ev: unknown) => void) | null;
  onerror: ((ev: unknown) => void) | null;
}

export type WebSocketFactory = (url: string) => WebSocketLike;

export interface ClientConfig {
  /** 网关地址，如 wss://example.com/v1/connect。 */
  url: string;
  /** 设备 ID（Hello.node_id）：须持久保存（如 localStorage），跨刷新与重启不变，在场与提醒按它区分设备。 */
  deviceId: string;
  /**
   * 返回业务线签发的用户令牌，每次连接时调用；令牌到期时网关断开连接，SDK 重连并重新取令牌
   * （docs/design/auth.md §4）。
   */
  tokenSource?: () => string | Promise<string>;
  /** 固定令牌，仅在 tokenSource 为空时使用（开发）。 */
  token?: string;
  businessLine?: string;
  endUser?: string;
  /** 人类可读的设备标签，如 "Chrome on MacBook"。 */
  label?: string;
  /** 设备类型，如 "browser"。 */
  kind?: string;
  hostApp?: string;
  /**
   * 推送通道与设备令牌（如 Web Push），给出时登记本设备接收提醒（§9）。pushToken 在每次连接时调用，
   * 返回空串时不登记。
   */
  pushPlatform?: string;
  pushToken?: () => string | Promise<string>;
  /** 创建 WebSocket；默认使用全局 WebSocket。小程序等环境在此适配。 */
  webSocket?: WebSocketFactory;
  /** 每次连接成功后调用，参数为网关分配的标签。 */
  onConnected?: (label: string) => void;
  /** 连接断开时调用（之后自动重连）。 */
  onDisconnected?: (reason: unknown) => void;
  /** 回调（SessionHandler、onConnected）抛出的异常；默认在下一个微任务中重新抛出。连接不受影响。 */
  onHandlerError?: (e: unknown) => void;
  /** 重连退避的初始与最大间隔（毫秒），默认 500 与 30000。 */
  minBackoffMs?: number;
  maxBackoffMs?: number;
}

/** 一个订阅的回调。按到达顺序串行调用，不应长时间阻塞；回调抛出的异常不影响连接。 */
export interface SessionHandler {
  onEvent(e: Event): void;
  /** 模型输出的实时增量：snapshot 为 true 时替换当前草稿，end 时丢弃未提交的草稿（见 Draft）。 */
  onDelta?(d: LiveDelta): void;
  /** 在场设备列表：订阅后先收到一次，之后每次变化时收到全量（包含本设备）。 */
  onPresence?(p: Presence): void;
  /** 网关结束了订阅："not_found"、"deleted"、"limit" 或 "error"（可重新订阅）。之后不再回调。 */
  onEnded?(reason: string): void;
}

/** 网关拒绝了请求；code 与 HTTP API 的状态码对应（"invalid"、"not_found"、"conflict"、"rejected" 等）。 */
export class RequestError extends Error {
  constructor(
    readonly code: string,
    message: string,
    /** code 为 "quota_exceeded" 时是配额重置时间。 */
    readonly retryAt?: Date,
  ) {
    super(`${code}: ${message}`);
    this.name = "RequestError";
  }
}

/**
 * 请求已发出，但连接在收到响应之前断开：结果未知。带输入 ID 的提交由 SDK 自动重试；
 * 其余请求重复执行不会产生重复效果（创建 Session 除外），由调用方决定是否重试。
 */
export class DisconnectedError extends Error {
  constructor() {
    super("connection lost before response; outcome unknown");
    this.name = "DisconnectedError";
  }
}

/** 客户端已停止（stop）。 */
export class StoppedError extends Error {
  constructor() {
    super("client stopped");
    this.name = "StoppedError";
  }
}

export interface RequestOptions {
  signal?: AbortSignal;
}

export interface SubmitResult {
  runId: string;
  /** 有活跃 Run，输入作为插话。 */
  steered: boolean;
  /** 非空表示输入作为对该 ask_user 提问的回答（Run 正在等用户回答）。 */
  answered: string;
  /** 同一输入 ID 的输入此前已生效，本次没有写入。 */
  duplicate: boolean;
}

export type ContentBlockInit = MessageInitShape<typeof ContentBlockSchema>;
export type ClientRequestInit = MessageInitShape<typeof ClientRequestSchema>;

interface Subscriber {
  /** 这个订阅者已收到的最后一个 seq：重新订阅与旧订阅的残留消息可能重叠，只交给它更新的事件。 */
  after: bigint;
  handler: SessionHandler;
}

/**
 * 一个 Session 在本连接上的订阅。subscribe 的订阅者至多一个（新的取代旧的）；渲染辅助（PendingTracker）对子 Session
 * 的订阅者与它并存、各自取消，这样应用打开子 Session 时不会挤掉辅助类的订阅，反之亦然。网关上一个 Session 只有一个订阅
 * （重复订阅以新的 after_seq 重新开始），从各订阅者中最小的 after 续传，每个订阅者各自丢弃已收到的事件。
 */
interface Subscription {
  primary?: Subscriber;
  observers: Set<Subscriber>;
  /** subscribe 的订阅者最近一次 setActivity 报告的前台状态，重连后随订阅重发（"正在输入"是瞬时的，不重发）。 */
  focused: boolean;
}

function members(sub: Subscription): Subscriber[] {
  return sub.primary ? [sub.primary, ...sub.observers] : [...sub.observers];
}

function minAfter(sub: Subscription): bigint {
  let after: bigint | undefined;
  for (const s of members(sub)) if (after === undefined || s.after < after) after = s.after;
  return after ?? 0n;
}

interface Pending {
  resolve(r: ClientResponse): void;
  reject(e: unknown): void;
}

export class Client {
  readonly #cfg: ClientConfig;
  readonly #subs = new Map<string, Subscription>();
  readonly #pending = new Map<string, Pending>();
  // 本连接上的 Call（开始中与进行中的），以 call_id 关联。
  readonly #calls = new Map<string, Call>();
  // 等待连接建立的请求。
  #waiters: Array<{ resolve(): void; reject(e: unknown): void }> = [];
  #ws: WebSocketLike | null = null;
  // 已收到 Welcome：可以发送订阅与请求。
  #ready = false;
  #started = false;
  #stopped = false;
  #nextId = 0;
  #wake: (() => void) | null = null;

  constructor(cfg: ClientConfig) {
    this.#cfg = cfg;
  }

  /** 开始连接；断开后自动重连，直到 stop。 */
  start(): void {
    if (this.#started) return;
    this.#started = true;
    void this.#loop();
  }

  /** 关闭连接并停止重连；未完成的请求以 StoppedError 结束。 */
  stop(): void {
    this.#stopped = true;
    this.#ws?.close(1000, "client stopped");
    this.#wake?.();
    this.#failPending(new StoppedError());
    for (const w of this.#waiters) w.reject(new StoppedError());
    this.#waiters = [];
  }

  get connected(): boolean {
    return this.#ready;
  }

  async #loop(): Promise<void> {
    const min = this.#cfg.minBackoffMs ?? 500;
    const max = this.#cfg.maxBackoffMs ?? 30_000;
    let backoff = min;
    while (!this.#stopped) {
      let reason: unknown;
      try {
        if (await this.#connection()) backoff = min;
      } catch (e) {
        reason = e;
      }
      if (this.#stopped) return;
      this.#cfg.onDisconnected?.(reason);
      const delay = backoff + Math.random() * (backoff / 2);
      await new Promise<void>((resolve) => {
        const t = setTimeout(resolve, delay);
        this.#wake = () => {
          clearTimeout(t);
          resolve();
        };
      });
      this.#wake = null;
      backoff = Math.min(backoff * 2, max);
    }
  }

  // #connection 处理一条连接直到断开；返回是否曾完成握手。
  async #connection(): Promise<boolean> {
    const cfg = this.#cfg;
    const token = cfg.tokenSource ? await cfg.tokenSource() : (cfg.token ?? "");
    const pushToken = cfg.pushToken ? await cfg.pushToken() : "";
    if (this.#stopped) return false;
    const factory = cfg.webSocket ?? ((url: string) => new WebSocket(url) as unknown as WebSocketLike);
    const ws = factory(cfg.url);
    ws.binaryType = "arraybuffer";
    this.#ws = ws;
    let welcomed = false;
    try {
      await new Promise<void>((resolve, reject) => {
        ws.onopen = () => {
          const hello = create(NodeMessageSchema, {
            msg: {
              case: "hello",
              value: {
                nodeId: cfg.deviceId,
                token,
                businessLine: cfg.businessLine ?? "",
                endUser: cfg.endUser ?? "",
                label: cfg.label ?? "",
                kind: cfg.kind ?? "",
                hostApp: cfg.hostApp ?? "",
                sdkVersion: `web/${VERSION}`,
                clientOnly: true,
                pushPlatform: pushToken ? (cfg.pushPlatform ?? "") : "",
                pushToken,
              },
            },
          });
          try {
            ws.send(toBinary(NodeMessageSchema, hello));
          } catch (e) {
            reject(e);
          }
        };
        ws.onmessage = (ev) => {
          let m: GatewayMessage;
          try {
            m = decode(ev.data);
          } catch (e) {
            ws.close(1002, "bad frame");
            reject(e);
            return;
          }
          if (!welcomed) {
            if (m.msg.case !== "welcome") {
              ws.close(1002, "expected welcome");
              reject(new Error(`expected welcome, got ${m.msg.case}`));
              return;
            }
            welcomed = true;
            try {
              this.#attach(ws);
            } catch (e) {
              ws.close(1011, "resubscribe failed");
              reject(e);
              return;
            }
            const label = m.msg.value.label;
            this.#call(() => cfg.onConnected?.(label));
            return;
          }
          this.#handle(m);
        };
        ws.onerror = () => {
          // 随后必有 onclose；在那里结束。
        };
        ws.onclose = (ev) => {
          const code = (ev as { code?: number } | undefined)?.code;
          reject(new Error(`connection closed${code ? ` (${code})` : ""}`));
        };
      });
    } catch (e) {
      if (!welcomed) throw e;
    } finally {
      this.#detach(ws);
    }
    return welcomed;
  }

  // #attach 在收到 Welcome 后调用：重新订阅全部 Session，并放行等待连接的请求。
  #attach(ws: WebSocketLike): void {
    for (const [sid, sub] of this.#subs) {
      ws.send(encode(subscribeMsg(sid, minAfter(sub))));
      if (sub.focused) ws.send(encode(activityMsg(sid, true, false)));
    }
    this.#ready = true;
    const waiters = this.#waiters;
    this.#waiters = [];
    for (const w of waiters) w.resolve();
  }

  // #detach 在连接断开时调用：未收到响应的请求以 DisconnectedError 结束。
  #detach(ws: WebSocketLike): void {
    if (this.#ws !== ws) return;
    ws.onopen = ws.onmessage = ws.onerror = ws.onclose = null;
    this.#ws = null;
    this.#ready = false;
    this.#failPending(new DisconnectedError());
    // Call 不跨连接：连接断开时网关已挂断（"disconnected"）。
    for (const c of [...this.#calls.values()]) c._ended("disconnected");
  }

  #failPending(e: unknown): void {
    const pending = [...this.#pending.values()];
    this.#pending.clear();
    for (const p of pending) p.reject(e);
  }

  #send(m: NodeMessage): void {
    if (this.#ready && this.#ws) this.#ws.send(encode(m));
  }

  // #call 调用一个回调；回调抛出的异常交给 onHandlerError，不中断连接的接收。
  #call(fn: () => void): void {
    try {
      fn();
    } catch (e) {
      const report = this.#cfg.onHandlerError;
      if (report) report(e);
      else
        queueMicrotask(() => {
          throw e;
        });
    }
  }

  #handle(m: GatewayMessage): void {
    switch (m.msg.case) {
      case "event": {
        const e = m.msg.value;
        // 重新订阅与旧订阅的残留消息可能重叠：每个订阅者只接受比自己已收到的更新的事件。
        for (const s of this.#members(e.sessionId)) {
          if (e.seq <= s.after || !this.#has(e.sessionId, s)) continue;
          s.after = e.seq;
          this.#call(() => s.handler.onEvent(e));
        }
        return;
      }
      case "delta": {
        const d = m.msg.value;
        for (const s of this.#members(d.sessionId)) {
          if (s.handler.onDelta && this.#has(d.sessionId, s)) this.#call(() => s.handler.onDelta!(d));
        }
        return;
      }
      case "presence": {
        const p = m.msg.value;
        for (const s of this.#members(p.sessionId)) {
          if (s.handler.onPresence && this.#has(p.sessionId, s)) this.#call(() => s.handler.onPresence!(p));
        }
        return;
      }
      case "subscriptionEnded": {
        const { sessionId, reason } = m.msg.value;
        const ended = this.#members(sessionId);
        this.#subs.delete(sessionId);
        for (const s of ended) if (s.handler.onEnded) this.#call(() => s.handler.onEnded!(reason));
        return;
      }
      case "callEvent": {
        const e = m.msg.value;
        const c = this.#calls.get(e.callId);
        if (c) this.#call(() => c._event(e));
        return;
      }
      case "response": {
        const r = m.msg.value;
        const p = this.#pending.get(r.requestId);
        this.#pending.delete(r.requestId);
        if (!p) return;
        if (r.error) {
          const at = r.error.retryAt ? timestampDate(r.error.retryAt) : undefined;
          p.reject(new RequestError(r.error.code, r.error.message, at));
        } else {
          p.resolve(r);
        }
        return;
      }
      default:
      // Node 角色的消息（Invoke 等）不会发给 client_only 连接。
    }
  }

  /**
   * 订阅 Session 中 seq 大于 after 的事件与之后的实时增量，返回取消函数。
   * 未连接时在连接建立后自动订阅；同一 Session 重复订阅时以新的为准。
   */
  subscribe(sessionId: string, after: bigint, handler: SessionHandler): () => void {
    const s: Subscriber = { after, handler };
    const sub = this.#subscription(sessionId);
    sub.primary = s;
    this.#send(subscribeMsg(sessionId, minAfter(sub)));
    return () => {
      const cur = this.#subs.get(sessionId);
      if (cur?.primary !== s) return;
      cur.primary = undefined;
      this.#release(sessionId, cur);
    };
  }

  /**
   * @internal 渲染辅助（PendingTracker）订阅子 Session：与 subscribe 的订阅者并存，互不取代，各自取消。
   */
  _observe(sessionId: string, after: bigint, handler: SessionHandler): () => void {
    const s: Subscriber = { after, handler };
    const sub = this.#subscription(sessionId);
    sub.observers.add(s);
    // 新的订阅者需要比已收到的更早的事件：网关上重新订阅，从最小的 after 续传；其他订阅者丢弃重放的事件。
    this.#send(subscribeMsg(sessionId, minAfter(sub)));
    return () => {
      const cur = this.#subs.get(sessionId);
      if (!cur?.observers.delete(s)) return;
      this.#release(sessionId, cur);
    };
  }

  #subscription(sessionId: string): Subscription {
    let sub = this.#subs.get(sessionId);
    if (!sub) {
      sub = { observers: new Set(), focused: false };
      this.#subs.set(sessionId, sub);
    }
    return sub;
  }

  #members(sessionId: string): Subscriber[] {
    const sub = this.#subs.get(sessionId);
    return sub ? members(sub) : [];
  }

  // 回调中可能取消了同一 Session 的其他订阅者：已取消的不再回调。
  #has(sessionId: string, s: Subscriber): boolean {
    const sub = this.#subs.get(sessionId);
    return !!sub && (sub.primary === s || sub.observers.has(s));
  }

  // #release 在一个订阅者取消后调用：没有订阅者时退订；subscribe 的订阅者走了而辅助类还在时，撤下前台状态，
  // 否则网关以为用户仍在看这个 Session，不再推送它的提醒。
  #release(sessionId: string, sub: Subscription): void {
    if (!sub.primary && sub.observers.size === 0) {
      this.#subs.delete(sessionId);
      this.#send(create(NodeMessageSchema, { msg: { case: "unsubscribe", value: { sessionId } } }));
      return;
    }
    if (!sub.primary && sub.focused) {
      sub.focused = false;
      this.#send(activityMsg(sessionId, false, false));
    }
  }

  /**
   * 报告本设备在一个已订阅 Session 中的状态：focused 表示该 Session 正显示在前台（有设备在看时不推送提醒），
   * typing 表示正在输入（网关 10 秒后视为停止，持续输入时每隔几秒重发）。未订阅的 Session 忽略。
   */
  setActivity(sessionId: string, focused: boolean, typing = false): void {
    const sub = this.#subs.get(sessionId);
    if (!sub?.primary) return;
    sub.focused = focused;
    this.#send(activityMsg(sessionId, focused, typing));
  }

  /** 发出一个请求并等待响应；未连接时等待连接建立。requestId 由 SDK 填写。 */
  async request(req: ClientRequestInit, opts: RequestOptions = {}): Promise<ClientResponse> {
    const { signal } = opts;
    while (!this.#ready) {
      if (this.#stopped) throw new StoppedError();
      await abortable(new Promise<void>((resolve, reject) => this.#waiters.push({ resolve, reject })), signal);
    }
    // 已取消的请求不发出：发出之后再取消只是不再等待，请求（如批准）仍会生效。
    signal?.throwIfAborted();
    const requestId = `r${++this.#nextId}`;
    const msg = create(NodeMessageSchema, { msg: { case: "request", value: { ...req, requestId } } });
    const p = new Promise<ClientResponse>((resolve, reject) => this.#pending.set(requestId, { resolve, reject }));
    try {
      this.#ws!.send(encode(msg));
    } catch (e) {
      this.#pending.delete(requestId);
      throw e;
    }
    return abortable(p, signal, () => this.#pending.delete(requestId));
  }

  /**
   * 在 Session 中开始一次 Call（全双工语音通话，docs/design/m3-call.md），接通（CallReady）后返回。
   * 同一 Session 中已有进行中的 Call 时，旧的随之结束；一条连接同一时刻至多一个 Call。
   * 失败（Session 不存在、Agent 不支持通话、配额用尽等）时以 RequestError 拒绝。
   */
  async startCall(sessionId: string, handler: CallHandler, opts: RequestOptions = {}): Promise<Call> {
    while (!this.#ready) {
      if (this.#stopped) throw new StoppedError();
      await abortable(new Promise<void>((resolve, reject) => this.#waiters.push({ resolve, reject })), opts.signal);
    }
    opts.signal?.throwIfAborted();
    const id = "call_" + newInputId().slice(3);
    const ws = this.#ws!;
    const c = new Call(id, sessionId, handler, (m) => {
      if (this.#ws === ws) ws.send(encode(m));
    });
    this.#calls.set(id, c);
    c._onEnd = () => this.#calls.delete(id);
    ws.send(encode(create(NodeMessageSchema, { msg: { case: "callStart", value: { callId: id, sessionId } } })));
    return abortable(c._ready, opts.signal, () => c.hangup());
  }

  /** 为连接的 EndUser 创建 Session；version 为空时按发布配置分流。返回 Session ID。 */
  async createSession(agent: string, version = "", opts?: RequestOptions): Promise<string> {
    const r = await this.request({ op: { case: "createSession", value: { agent, agentVersion: version } } }, opts);
    return r.sessionId;
  }

  /**
   * 提交输入：有活跃 Run 时作为插话，Run 正在等用户回答提问时作为回答，否则开启新 Run。
   * SDK 为每次提交生成输入 ID，连接在响应之前断开时，重连后以同一 ID 自动重试，输入恰好生效一次。
   */
  submit(sessionId: string, input: ContentBlockInit[], opts?: RequestOptions): Promise<SubmitResult> {
    return this.submitInput(sessionId, input, newInputId(), opts);
  }

  /** 提交一段文本，可附带界面上下文（见 uiContext）。 */
  submitText(sessionId: string, s: string, opts?: RequestOptions & { uiContext?: UIContextInit }): Promise<SubmitResult> {
    const input: ContentBlockInit[] = [text(s)];
    if (opts?.uiContext) input.push(uiContext(opts.uiContext));
    return this.submit(sessionId, input, opts);
  }

  /**
   * 以给定的输入 ID 提交。需要在页面刷新后继续重试的应用，自己生成并持久保存 ID（newInputId）。
   * inputId 为空时不去重、断线不重试。
   */
  async submitInput(sessionId: string, input: ContentBlockInit[], inputId: string, opts?: RequestOptions): Promise<SubmitResult> {
    for (;;) {
      try {
        const r = await this.request({ op: { case: "submit", value: { sessionId, input, inputId } } }, opts);
        return { runId: r.runId, steered: r.steered, answered: r.answered, duplicate: r.duplicate };
      } catch (e) {
        if (e instanceof DisconnectedError && inputId !== "") continue; // request 会等到重新连接
        throw e;
      }
    }
  }

  async interrupt(sessionId: string, runId: string, opts?: RequestOptions): Promise<void> {
    await this.request({ op: { case: "interrupt", value: { sessionId, runId } } }, opts);
  }

  async decide(sessionId: string, callId: string, approve: boolean, opts?: RequestOptions): Promise<void> {
    await this.request({ op: { case: "decide", value: { sessionId, callId, approve } } }, opts);
  }

  /**
   * 回答 Agent 的 ask_user 提问（见 questionOf）：selected 是点选的选项 ID，values 是填写的字段，
   * text 是输入的文字，可以只给出其中一部分。不合法时以 RequestError("invalid") 拒绝；已被回答时为 "conflict"。
   */
  async answer(
    sessionId: string,
    callId: string,
    a: { selected?: string[]; values?: Record<string, string>; text?: string },
    opts?: RequestOptions,
  ): Promise<void> {
    await this.request({ op: { case: "answer", value: { sessionId, callId, ...a } } }, opts);
  }

  /**
   * 提交用户在卡片上的动作（点按钮或点选列表项，docs/design/cards.md §4），values 是卡片上字段的填写值。
   * 有进行中的 Run 时并入它，否则开启新 Run；同一版卡片上的同一动作只生效一次（重复点击返回 duplicate）。
   * seq 是收到这一版卡片的事件序号（见 Cards），平台据此直接读取卡片。
   */
  submitCardAction(
    sessionId: string,
    a: { cardId: string; callId: string; action: string; values?: Record<string, string>; seq?: bigint },
    opts?: RequestOptions,
  ): Promise<SubmitResult> {
    return this.submit(sessionId, [cardAction(a)], opts);
  }

  async closeSession(sessionId: string, reason = "", opts?: RequestOptions): Promise<void> {
    await this.request({ op: { case: "close", value: { sessionId, reason } } }, opts);
  }
}

export interface UIContextInit {
  /** HostApp 中的界面，如 "订单详情"（≤100 字符）。 */
  screen?: string;
  /** 界面对应的业务对象或链接，如 "order://A1029"（≤500）。 */
  ref?: string;
  /** 用户选中的文字（≤2000）。 */
  selection?: string;
  /** 界面上与输入相关的可见内容（≤8000）。 */
  content?: string;
}

/** 卡片动作内容块（见 submitCardAction）。 */
export function cardAction(a: {
  cardId: string;
  callId: string;
  action: string;
  values?: Record<string, string>;
  seq?: bigint;
}): ContentBlockInit {
  return {
    kind: { case: "cardAction", value: { cardId: a.cardId, callId: a.callId, action: a.action, values: a.values ?? {}, seq: a.seq ?? 0n } },
  };
}

/** 文本内容块。 */
export function text(s: string): ContentBlockInit {
  return { kind: { case: "text", value: { text: s } } };
}

/**
 * 界面上下文内容块（§8）：与用户的话一并提交，帮助模型理解"这个""这里"指什么。
 * 每条输入至多一个，且不能只有界面上下文；超出长度上限时提交被拒绝（invalid）。
 */
export function uiContext(c: UIContextInit): ContentBlockInit {
  return { kind: { case: "uiContext", value: c } };
}

/** 生成一个随机的输入 ID。 */
export function newInputId(): string {
  const b = new Uint8Array(16);
  globalThis.crypto.getRandomValues(b);
  return "in_" + Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
}

/** 内容块中的全部文本。 */
export function textOf(blocks: readonly ContentBlock[]): string {
  return blocks.map((b) => (b.kind.case === "text" ? b.kind.value.text : "")).join("");
}

function subscribeMsg(sessionId: string, afterSeq: bigint): NodeMessage {
  return create(NodeMessageSchema, { msg: { case: "subscribe", value: { sessionId, afterSeq } } });
}

function activityMsg(sessionId: string, focused: boolean, typing: boolean): NodeMessage {
  return create(NodeMessageSchema, { msg: { case: "activity", value: { sessionId, focused, typing } } });
}

function encode(m: NodeMessage): Uint8Array {
  return toBinary(NodeMessageSchema, m);
}

function decode(data: unknown): GatewayMessage {
  if (data instanceof ArrayBuffer) return fromBinary(GatewayMessageSchema, new Uint8Array(data));
  if (data instanceof Uint8Array) return fromBinary(GatewayMessageSchema, data);
  throw new Error("unexpected frame type");
}

function abortable<T>(p: Promise<T>, signal: AbortSignal | undefined, onAbort?: () => void): Promise<T> {
  if (!signal) return p;
  if (signal.aborted) {
    onAbort?.();
    return Promise.reject(signal.reason);
  }
  return new Promise<T>((resolve, reject) => {
    const abort = () => {
      onAbort?.();
      reject(signal.reason);
    };
    signal.addEventListener("abort", abort, { once: true });
    p.then(
      (v) => {
        signal.removeEventListener("abort", abort);
        resolve(v);
      },
      (e) => {
        signal.removeEventListener("abort", abort);
        reject(e);
      },
    );
  });
}

/** 一次 Call 的回调。按到达顺序串行调用。 */
export interface CallHandler {
  /** 助手的语音：PCM 16 位、单声道，采样率见 Call.outputSampleRate（24000）。 */
  onAudio(pcm: Int16Array, responseId: string): void;
  /** 检测到用户开始说话：立即停止播放尚未播完的音频（打断），并丢弃该回复迟到的音频。 */
  onSpeech?(): void;
  /** 转写：用户的话（final 为 false 时是识别中的整句，替换上一条）与助手的话（final 为 false 时是增量）。 */
  onText?(t: CallText): void;
  onResponseDone?(responseId: string): void;
  /** 通话中派生的后台任务的进展。 */
  onTask?(t: CallTask): void;
  /** Call 结束："hangup"、"disconnected"、"provider"、"replaced"、"session_closed"、"quota"、"error"。 */
  onEnded?(reason: string): void;
}

/** 进行中的 Call。上行音频为 PCM 16 位、单声道、16000 Hz，按实时节奏发送，建议每帧 20 ms（320 个采样）。 */
export class Call {
  readonly inputSampleRate = 16000;
  outputSampleRate = 24000;
  model = "";
  #ended = false;
  #resolve!: (c: Call) => void;
  #reject!: (e: unknown) => void;
  /** @internal */
  readonly _ready: Promise<Call>;
  /** @internal */
  _onEnd: () => void = () => {};

  constructor(
    readonly id: string,
    readonly sessionId: string,
    readonly handler: CallHandler,
    readonly send: (m: NodeMessage) => void,
  ) {
    this._ready = new Promise((resolve, reject) => {
      this.#resolve = resolve;
      this.#reject = reject;
    });
    // 未被等待时（abort 之后）不产生未处理的拒绝。
    this._ready.catch(() => {});
  }

  get ended(): boolean {
    return this.#ended;
  }

  /** 发送麦克风音频。 */
  sendAudio(pcm: Int16Array | Uint8Array): void {
    if (this.#ended) return;
    const bytes = pcm instanceof Uint8Array ? pcm : new Uint8Array(pcm.buffer, pcm.byteOffset, pcm.byteLength);
    this.send(create(NodeMessageSchema, { msg: { case: "callAudio", value: { callId: this.id, pcm: bytes } } }));
  }

  /** 关闭 / 打开麦克风：静音期间无需发送音频。 */
  mute(muted: boolean): void {
    this.#control(muted ? CallAction.MUTE : CallAction.UNMUTE);
  }

  /** 在界面上打断助手的播报（说话打断由模型检测，无需调用）。 */
  interrupt(): void {
    this.#control(CallAction.INTERRUPT);
  }

  hangup(): void {
    this.#control(CallAction.HANGUP);
  }

  #control(action: CallAction): void {
    if (this.#ended) return;
    try {
      this.send(create(NodeMessageSchema, { msg: { case: "callControl", value: { callId: this.id, action } } }));
    } catch {
      // 连接已断开：Call 随之结束，无需控制。
    }
  }

  /** @internal */
  _event(e: CallEvent): void {
    const h = this.handler;
    switch (e.kind.case) {
      case "ready":
        this.#onReady(e.kind.value);
        return;
      case "audio": {
        const b = e.kind.value.pcm;
        // 复制到对齐的缓冲区：protobuf 解出的字节可能不按 2 字节对齐。
        const pcm = new Int16Array(b.byteLength >> 1);
        new Uint8Array(pcm.buffer).set(b.subarray(0, pcm.byteLength));
        h.onAudio(pcm, e.kind.value.responseId);
        return;
      }
      case "speech":
        h.onSpeech?.();
        return;
      case "text":
        h.onText?.(e.kind.value);
        return;
      case "responseDone":
        h.onResponseDone?.(e.kind.value.responseId);
        return;
      case "task":
        h.onTask?.(e.kind.value);
        return;
      case "ended":
        this._ended(e.kind.value.reason);
        return;
      case "error": {
        const err = e.kind.value;
        this.#ended = true;
        this._onEnd();
        this.#reject(new RequestError(err.code, err.message, err.retryAt ? timestampDate(err.retryAt) : undefined));
        return;
      }
    }
  }

  #onReady(r: CallReady): void {
    this.outputSampleRate = r.outputSampleRate || 24000;
    this.model = r.model;
    this.#resolve(this);
  }

  /** @internal */
  _ended(reason: string): void {
    if (this.#ended) return;
    this.#ended = true;
    this._onEnd();
    this.#reject(new Error(`call ended: ${reason}`));
    this.handler.onEnded?.(reason);
  }
}
