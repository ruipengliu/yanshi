// 对话组件：一台模拟设备上一个 Session 的聊天界面（"对话"视图全屏使用，"设备"视图中每台设备一个迷你版）。
// 只用公开协议：事件来自订阅（SessionView），输入、卡片动作、审批、回答、中断与通话都经这台设备的 Connection 发出，
// 附件经 HTTP API 上传为工件。多台设备打开同一个 Session 时，它们看到的是同一份事件。
//
// 助手的一轮（同一个 Run）依次显示：思考过程（可折叠，生成中实时展开）、正文、本次调用（折叠成一行）、模型信息行、
// 卡片。点调用、模型信息行或卡片，在详情抽屉中查看全部信息（ctx.onSelect）。

import { Mic, Player, Recorder } from "./audio.js";
import { renderApproval, renderCard, renderQuestion } from "./cards.js";
import { RequestError, newInputId, payloadOf, routeInput, textOf } from "./device.js";
import { render as markdown } from "./markdown.js";
import { bytes, clock, copyButton, dur, fill, h, hue, num, reconcile, short, toast } from "./ui.js";

/** 工具名去掉设备标签前缀后的能力名与目标："iphone__set_alarm" → ["set_alarm", "iphone"]。 */
export function splitTool(name) {
  const i = String(name).indexOf("__");
  return i < 0 ? [name, ""] : [name.slice(i + 2), name.slice(0, i)];
}

export const TOOL_ICON = {
  set_alarm: "⏰",
  list_alarms: "⏰",
  cancel_alarm: "⏰",
  create_event: "📅",
  list_events: "📅",
  start_timer: "⏱️",
  send_sms: "✉️",
  take_photo: "📷",
  get_location: "📍",
  play_music: "🎵",
  set_volume: "🔊",
  navigate: "🧭",
  set_climate: "❄️",
  get_heart_rate: "❤️",
  read_file: "📄",
  list_dir: "📁",
  write_file: "📝",
  open_url: "🌐",
  run_python: "🐍",
  clock_now: "🕒",
  memory_save: "🧠",
  memory_search: "🧠",
  memory_forget: "🧠",
  show_card: "🃏",
};

/** 调用的显示名与图标：委托显示为"委托 <Agent>"。 */
export function toolLabel(capability) {
  const [cap, target] = splitTool(capability);
  if (target === "agent") return { icon: "🤝", name: `委托 ${cap}`, target: "", delegate: true };
  return { icon: TOOL_ICON[cap] ?? "🔧", name: cap, target, delegate: false };
}

/**
 * callIndex 汇总每个调用的进展：call_id → {capability, args, runId, attempt, asked, started, startedAt, nodeId,
 * executions, done, error, result, blocks, doneAt, approval, summary, runEnded}。runEnded 是所属 Run 的终态
 * （run_completed、run_failed、run_interrupted），Run 尚未结束时为空。
 */
export function callIndex(view) {
  const out = new Map();
  const ended = new Map();
  const get = (id) => {
    if (!out.has(id)) out.set(id, { executions: 0 });
    return out.get(id);
  };
  for (const e of view.events) {
    const [kind, p] = payloadOf(e);
    const t = Date.parse(e.time);
    if (kind === "assistant_message") {
      for (const c of p.tool_calls ?? [])
        Object.assign(get(c.call_id), {
          capability: c.capability,
          args: c.arguments_json,
          runId: p.run_id,
          attempt: p.attempt ?? 0,
          askedAt: t,
        });
    } else if (kind === "tool_call_started") {
      const c = get(p.call_id);
      c.executions++;
      Object.assign(c, { started: true, startedAt: c.startedAt ?? t, nodeId: p.node_id ?? "", attempt: p.attempt ?? c.attempt });
    } else if (kind === "tool_result")
      Object.assign(get(p.call_id), { done: true, error: !!p.is_error, result: textOf(p.content), blocks: p.content, doneAt: t });
    else if (kind === "approval_requested") Object.assign(get(p.call_id), { approval: "pending", summary: p.summary, approvalAt: t });
    else if (kind === "approval_decided") Object.assign(get(p.call_id), { approval: p.approved ? "approved" : "denied", decidedAt: t });
    else if (kind === "run_completed" || kind === "run_failed" || kind === "run_interrupted") ended.set(p.run_id, kind);
  }
  for (const c of out.values()) c.runEnded = ended.get(c.runId) ?? "";
  return out;
}

/**
 * 调用的状态：queued、running、approval、denied、error、done、aborted。Run 中断或失败时，还没有结果的调用不会再有结果：
 * 显示为 aborted（已终止），而不是一直"执行中""待审批"。已有结果的调用保留原状态。
 */
export function callState(st = {}) {
  if (!st.done && st.runEnded && st.approval !== "denied") return "aborted";
  if (st.approval === "pending" && !st.done) return "approval";
  if (st.approval === "denied") return "denied";
  if (st.done) return st.error ? "error" : "done";
  if (st.started) return "running";
  return "queued";
}

const STATE_TEXT = {
  queued: "排队",
  running: "执行中",
  approval: "待审批",
  denied: "已拒绝",
  error: "出错",
  done: "完成",
  aborted: "已终止",
};

/** 模型信息行的文字：模型 · 输入（缓存）· 输出 · 思考 · 首字 · 耗时 · 结束原因。 */
export function modelLine(m) {
  const u = m.usage ?? {};
  const bits = [m.model ?? "?"];
  if (u.input_tokens) bits.push(`输入 ${num(u.input_tokens)}${u.cached_input_tokens ? `（缓存 ${num(u.cached_input_tokens)}）` : ""}`);
  if (u.output_tokens) bits.push(`输出 ${num(u.output_tokens)}`);
  if (u.reasoning_tokens) bits.push(`思考 ${num(u.reasoning_tokens)}`);
  if (m.first_token_ms) bits.push(`首字 ${dur(m.first_token_ms)}`);
  if (m.duration_ms) bits.push(`耗时 ${dur(m.duration_ms)}`);
  const fin = { stop: "结束", tool_calls: "工具调用", length: "长度截断", content_filter: "内容过滤" }[m.finish_reason] ?? m.finish_reason;
  if (fin) bits.push(fin);
  return bits.join(" · ");
}

export class ChatPanel {
  /**
   * @param {object} o
   *   device   SimDevice（会话客户端）
   *   view     该设备上的 SessionView
   *   ctx      {artifacts, user, children(sessionId), onSelect(sel), selected(), onTimeline(runId), toggleDrawer()}
   *   compact  迷你版：没有页头、通话与说明
   */
  constructor({ device, view, ctx, compact = false }) {
    this.device = device;
    this.view = view;
    this.ctx = ctx;
    this.compact = compact;
    this.attachments = [];
    // 上传中的附件（Promise）：发送等它们完成，否则文件留给下一条，文字也可能被当作子 Agent 提问的回答。
    this.uploads = new Set();
    // 上次发送失败的输入（内容与 input_id）：原样再发时沿用同一个 input_id，服务端按它去重。
    this.failed = null;
    // 卡片字段（card_id/call_id → 值）与提问（sessionId/callId → 选项、字段、文字）的填写：同一 Run 的后续消息会重建
    // 整段轮次与卡片，填写的值保存在这里，重建时恢复。
    this.cardValues = new Map();
    this.questionState = new Map();
    this.call = null;
    this.queued = false;
    this.seen = 0;
    this.root = h("div", { class: `chatpane ${compact ? "compact" : ""}` });
    this.header = h("div", { class: "chat-header" });
    this.callBar = h("div", { class: "call-bar", hidden: true });
    this.log = h("div", { class: "messages" });
    // 已提交的内容只在有新事件时重建；正在生成的草稿单独放在 draftEl，按帧平滑地更新（renderDraft）。
    this.committedEl = h("div", { class: "committed" });
    this.draftEl = h("div", { class: "draft-area" });
    this.log.append(h("div", { class: "conversation" }, this.committedEl, this.draftEl));
    // 用户展开或收起的思考块、调用组（data-key → open），重建后保持。
    this.openState = new Map();
    this.log.addEventListener(
      "toggle",
      (ev) => {
        const key = ev.target?.dataset?.key;
        if (key && !ev.target.dataset.auto) this.openState.set(key, ev.target.open);
        if (ev.target?.dataset) delete ev.target.dataset.auto;
      },
      true,
    );
    // 平滑输出：草稿中已显示的部分（shown）按稳定节奏追赶收到的部分（view.draft）。
    this.shown = { key: "", text: "", reasoning: "" };
    this.dirty = new Set(["event"]);
    this.newBtn = h("button", { class: "new-messages", hidden: true, onclick: () => this.scrollBottom() }, "↓ 新消息");
    this.status = h("div", { class: "composer-status" });
    this.pending = h("div", { class: "pending" });
    // 进行中的委托的子 Session：它们的审批与提问也在这里展示（renderPending），变化时重绘。sid → 取消订阅。
    this.childOffs = new Map();
    this.pendingKey = "";
    this.waitingUser = new Set();
    // 用 fill 而不是 append：append(null) 会插入文字 "null"。
    fill(
      this.root,
      compact ? null : this.header,
      this.callBar,
      h("div", { class: "messages-wrap" }, this.log, this.newBtn),
      this.composer(),
    );
    this.log.addEventListener("scroll", () => this.nearBottom() && (this.newBtn.hidden = true));
    this.off = view.on((kind) => this.schedule(kind));
    this.offDev = device.on((n) => {
      if (n.type === "status") {
        this.schedule("status");
        // 连接断开时网关已结束通话：停止采集与播放，不再发 HANGUP。
        if (this.call && this.device.status !== "online") this.endCall(null);
      }
      if (n.type === "call") this.onCallEvent(n.event);
    });
    this.root.addEventListener("dragover", (e) => e.preventDefault());
    this.root.addEventListener("drop", (e) => {
      e.preventDefault();
      this.addFiles([...(e.dataTransfer?.files ?? [])]);
    });
    this.render();
    requestAnimationFrame(() => this.scrollBottom());
  }

  dispose() {
    this.off();
    this.offDev();
    for (const off of this.childOffs.values()) off();
    this.endCall("hangup");
    // 面板销毁时正在录的语音消息丢弃，并释放麦克风。
    this.recorder?.stop();
    this.recorder = null;
  }

  /**
   * schedule 在下一帧重绘：kind 为 "draft" 时只更新草稿区，"pending" 时只更新待审批与待回答（子 Session 的变化），
   * 其余（事件、在场、连接）重建已提交的内容。
   */
  schedule(kind = "event") {
    this.dirty.add(kind);
    if (this.queued) return;
    this.queued = true;
    requestAnimationFrame((t) => {
      this.queued = false;
      this.render(t);
    });
  }

  nearBottom() {
    return this.log.scrollHeight - this.log.scrollTop - this.log.clientHeight < 80;
  }

  scrollBottom() {
    this.log.scrollTop = this.log.scrollHeight;
    this.newBtn.hidden = true;
  }

  /** 中断进行中的 Run（Esc）。 */
  interrupt() {
    const run = this.view.activeRun();
    if (run) this.device.interrupt(this.view.sessionId, run).catch((e) => toast(e.message, "error"));
  }

  // —— 渲染 ——

  render(now = performance.now()) {
    const near = this.nearBottom();
    const before = this.log.scrollHeight;
    // 在场的变化（谁在看、正在输入）只更新状态行，不重建对话：重建会打断正在进行的选择与滚动。
    const committed = [...this.dirty].some((k) => k !== "draft" && k !== "presence" && k !== "pending");
    const pending = committed || this.dirty.has("pending");
    this.dirty.clear();
    if (committed) {
      const blocks = this.blocks();
      reconcile(
        this.committedEl,
        blocks.length || this.view.draft.runId
          ? blocks
          : [h("div", { class: "empty" }, this.compact ? "还没有消息" : "发一条消息开始对话。可以附图片、发语音，或者打电话。")],
      );
      if (!this.compact) this.renderHeader();
    }
    if (pending) this.renderPending();
    const behind = this.renderDraft(now);
    if (near) this.log.scrollTop = this.log.scrollHeight;
    else if (committed && this.log.scrollHeight > before && this.view.events.length !== this.seen) this.newBtn.hidden = false;
    this.seen = this.view.events.length;
    this.renderStatus();
    // 草稿还没追上收到的内容：下一帧继续。
    if (behind) this.schedule("draft");
  }

  /**
   * renderDraft 把草稿中已显示的部分向收到的部分推进一帧并重绘草稿区，返回是否还没追上。速度随积压自适应：至少每秒
   * 40 字，积压多时在约 250ms 内追上，所以模型一阵一阵地吐字时，界面上仍是均匀的。
   */
  renderDraft(now) {
    const d = this.view.draft;
    const key = `${d.runId}/${d.attempt}/${d.afterSeq}`;
    const s = this.shown;
    if (!d.runId || (!d.text && !d.reasoning)) {
      s.key = "";
      s.text = s.reasoning = "";
      this.draftEl.replaceChildren();
      return false;
    }
    if (s.key !== key) Object.assign(s, { key, text: "", reasoning: "", at: now });
    const dt = Math.min(Math.max(now - (s.at ?? now), 0), 100);
    s.at = now;
    const advance = (cur, target) => {
      if (!target.startsWith(cur)) return target.slice(0, Math.min(cur.length, target.length)); // snapshot 替换了草稿
      const backlog = target.length - cur.length;
      if (backlog <= 0) return cur;
      let n = Math.max(1, Math.ceil(Math.max((backlog * dt) / 250, (40 * dt) / 1000)));
      const code = target.charCodeAt(cur.length + n - 1);
      if (code >= 0xd800 && code <= 0xdbff) n++; // 不切开代理对（emoji）
      return target.slice(0, cur.length + n);
    };
    // 先显示思考过程，再显示正文：正文开始时思考已全部显示。
    s.reasoning = advance(s.reasoning, d.reasoning ?? "");
    s.text = s.reasoning.length >= (d.reasoning ?? "").length ? advance(s.text, d.text ?? "") : "";
    const parts = [];
    if (s.reasoning) parts.push(this.thinking(s.reasoning, !s.text, true, undefined, `draft-think:${key}`));
    if (s.text) parts.push(h("div", { class: "prose md" }, markdown(s.text, this.mdCtx()), h("span", { class: "cursor" })));
    const continued = this.lastTurnRun === d.runId;
    fill(
      this.draftEl,
      parts.length
        ? h(
            "div",
            { class: `assistant-turn draft ${continued ? "continued" : ""}`, style: { "--run": hue(d.runId) } },
            this.compact ? null : h("div", { class: "avatar" }, continued ? "" : "◆"),
            h(
              "div",
              { class: "turn-main" },
              this.compact || continued ? null : h("div", { class: "speaker" }, h("b", {}, this.ctx.agentName?.() ?? "助手"), h("code", { class: "run-id" }, short(d.runId))),
              h("div", { class: "turn-body" }, parts),
            ),
          )
        : null,
    );
    // 思考区有最大高度：生成中保持滚动在末尾，看得到最新的思考。
    const live = this.draftEl.querySelector(".thinking[open] .thinking-text");
    if (live) live.scrollTop = live.scrollHeight;
    return s.reasoning.length < (d.reasoning ?? "").length || s.text.length < (d.text ?? "").length;
  }

  /** blocks 把事件组织成用户消息与助手的轮次。 */
  blocks() {
    const v = this.view;
    const calls = callIndex(v);
    const cards = v.cards();
    const taken = v.cardActions();
    const versions = new Map();
    for (const e of v.events)
      for (const b of e.tool_result?.content ?? []) if (b.card) versions.set(b.card.card_id, (versions.get(b.card.card_id) ?? 0) + 1);
    const fromCall = new Set();
    const out = [];
    let turn = null;
    const turnFor = (runId, e) => {
      if (turn?.runId !== runId) {
        turn = { runId, body: h("div", { class: "turn-body" }) };
        out.push(this.turnShell(turn, e));
      }
      return turn.body;
    };
    for (const e of v.events) {
      const [kind, p] = payloadOf(e);
      switch (kind) {
        case "run_requested":
        case "steered":
          if (p.from_call) fromCall.add(p.run_id);
          out.push(this.userBubble(p, e));
          turn = null;
          break;
        case "assistant_message":
          turnFor(p.run_id, e).append(...this.step(p, e, calls, fromCall.has(p.run_id)));
          break;
        case "tool_result":
          for (const b of p.content ?? []) {
            if (b.card) {
              const latest = cards.get(b.card.card_id);
              if (latest && latest.card.call_id !== b.card.call_id) continue; // 只显示最新版
              const sel = this.ctx.selected?.();
              turnFor(p.run_id, e).append(
                h(
                  "div",
                  {
                    class: `card-slot ${sel?.kind === "card" && sel.cardId === b.card.card_id ? "selected" : ""}`,
                    onclick: (ev) =>
                      !ev.target.closest("button,input,select,a") && this.ctx.onSelect?.({ kind: "card", cardId: b.card.card_id }),
                  },
                  renderCard(b.card, {
                    ...this.mdCtx(),
                    values: keep(this.cardValues, `${b.card.card_id}/${b.card.call_id}`, () => ({})),
                    taken,
                    history: (versions.get(b.card.card_id) ?? 1) - 1,
                    onAction: (action, values) => this.cardAction(b.card, action, values, e),
                  }),
                ),
              );
            } else if (b.media && !p.attempt) {
              turnFor(p.run_id, e).append(h("div", { class: "device-media" }, h("div", { class: "tag" }, "设备返回"), this.media(b.media)));
            }
          }
          break;
        case "call_transcript":
          out.push(
            h(
              "div",
              { class: `voice-line ${p.role === "assistant" ? "assistant" : "user"}` },
              h("span", { class: "tag" }, p.role === "assistant" ? "🎙 助手" : "🎙 你"),
              p.text,
              p.interrupted ? "……" : "",
            ),
          );
          turn = null;
          break;
        case "call_started":
          out.push(h("div", { class: "note" }, `📞 通话开始 · ${p.model ?? ""}`));
          break;
        case "call_ended":
          out.push(h("div", { class: "note" }, `通话结束：${p.reason ?? ""}`));
          break;
        case "run_failed":
          turnFor(p.run_id, e).append(h("div", { class: "note error" }, `Run 失败：${p.reason ?? ""}`));
          break;
        case "run_interrupted":
          turnFor(p.run_id, e).append(h("div", { class: "note" }, "已中断"));
          break;
        case "content_moderated":
          turnFor(p.run_id, e).append(h("div", { class: "note error" }, `内容安全拦截（${p.stage}）`));
          break;
        case "context_compacted":
          turnFor(p.run_id, e).append(h("div", { class: "note" }, "上下文已压缩"));
          break;
        case "memory_recalled":
          if ((p.items ?? []).length) turnFor(p.run_id, e).append(h("div", { class: "note" }, `🧠 召回 ${p.items.length} 条记忆`));
          break;
        case "agent_switched":
          out.push(h("div", { class: "note" }, `Agent 切换：${p.from?.version ?? ""} → ${p.to?.version ?? ""}`));
          break;
      }
    }
    this.lastTurnRun = turn?.runId ?? "";
    return out;
  }

  turnShell(turn, e) {
    const agent = this.ctx.agentName?.() ?? "助手";
    return h(
      "div",
      { class: "assistant-turn", style: { "--run": hue(turn.runId) } },
      this.compact ? null : h("div", { class: "avatar" }, "◆"),
      h(
        "div",
        { class: "turn-main" },
        this.compact
          ? null
          : h(
              "div",
              { class: "speaker" },
              h("b", {}, agent),
              h("time", {}, clock(e.time)),
              h("code", { class: "run-id", title: turn.runId }, short(turn.runId)),
            ),
        turn.body,
      ),
    );
  }

  /** 一次模型调用：思考、正文、调用、模型信息行。 */
  step(p, e, calls, fromCall) {
    const parts = [];
    if (p.reasoning) parts.push(this.thinking(p.reasoning, false, false, p.usage?.reasoning_tokens, `think:${e.seq}`));
    const text = textOf(p.content);
    if (text) {
      parts.push(
        h(
          "div",
          { class: "prose md hoverable" },
          fromCall ? h("div", { class: "tag" }, "后台任务") : null,
          markdown(text, this.mdCtx()),
          this.compact
            ? null
            : h(
                "div",
                { class: "msg-tools" },
                copyButton(text),
                h("button", { type: "button", onclick: () => this.ctx.onTimeline?.(p.run_id) }, "在时间线中查看"),
              ),
        ),
      );
    }
    const tools = (p.tool_calls ?? []).filter((c) => c.capability !== "ask_user");
    if (tools.length) parts.push(this.callGroup(tools, calls, p.run_id, `calls:${e.seq}`));
    const sel = this.ctx.selected?.();
    if (p.model || p.usage)
      parts.push(
        h(
          "button",
          {
            class: `model-info ${sel?.kind === "model" && sel.seq === String(e.seq) ? "selected" : ""}`,
            title: "查看这次模型调用的详情",
            onclick: () => this.ctx.onSelect?.({ kind: "model", seq: String(e.seq) }),
          },
          modelLine(p),
        ),
      );
    return parts;
  }

  thinking(text, open, live, tokens, key) {
    const userSet = key && this.openState.has(key);
    const d = h(
      "details",
      { class: `thinking ${live ? "live" : ""}`, open: userSet ? this.openState.get(key) : open, dataset: key ? { key } : undefined },
      h(
        "summary",
        {},
        live && open ? h("span", { class: "pulse" }) : "💭",
        live && open ? " 正在思考…" : ` 思考过程${tokens ? ` · ${num(tokens)} tokens` : ` · ${text.length} 字`}`,
      ),
      h("div", { class: "thinking-text" }, text, live && open ? h("span", { class: "cursor" }) : null),
    );
    // 程序设定的展开状态不算用户的选择（toggle 事件在插入后异步触发）。
    if (d.open && !userSet) d.dataset.auto = "1";
    return d;
  }

  /**
   * callGroup 是一次模型调用发起的调用：一个时直接是一行，多个时折叠成"N 个调用"一行。被后续同名调用改正的出错调用
   * （如参数不合法后重试）弱化显示。
   */
  callGroup(tools, calls, runId, key) {
    const sel = this.ctx.selected?.();
    const runCalls = [...calls.values()].filter((c) => c.runId === runId);
    const rows = tools.map((c) => {
      const st = calls.get(c.call_id) ?? {};
      const state = callState(st);
      const retried =
        state === "error" && runCalls.some((o) => o.capability === c.capability && o.askedAt > st.askedAt && o.done && !o.error);
      const l = toolLabel(c.capability);
      const took = st.doneAt && st.askedAt ? dur(st.doneAt - st.askedAt) : "";
      return h(
        "button",
        {
          class: `call-row ${state} ${retried ? "retried" : ""} ${l.delegate ? "delegate" : ""} ${sel?.kind === "call" && sel.callId === c.call_id ? "selected" : ""}`,
          title: retried ? "出错后模型重试成功" : "查看调用详情",
          onclick: () => this.ctx.onSelect?.({ kind: "call", callId: c.call_id }),
        },
        h("span", { class: "call-icon" }, l.icon),
        h("code", {}, l.name),
        l.target ? h("span", { class: "muted" }, "@", l.target) : null,
        l.delegate && st.nodeId ? h("span", { class: "muted" }, "子 Session ", short(st.nodeId.slice(7))) : null,
        h(
          "span",
          { class: "row-status" },
          retried ? "出错，已重试" : STATE_TEXT[state],
          took && !retried ? h("span", { class: "took" }, took) : null,
        ),
      );
    });
    if (rows.length === 1) return h("div", { class: "calls single" }, rows);
    const states = tools.map((c) => callState(calls.get(c.call_id)));
    const live = states.filter((s) => s === "running" || s === "queued" || s === "approval").length;
    const errs = states.filter((s) => s === "error" || s === "denied").length;
    const aborted = states.filter((s) => s === "aborted").length;
    const span = (() => {
      const xs = tools.map((c) => calls.get(c.call_id)).filter((s) => s?.doneAt);
      return xs.length ? dur(Math.max(...xs.map((s) => s.doneAt)) - Math.min(...xs.map((s) => s.askedAt))) : "";
    })();
    const summary = live ? `${live} 个进行中` : errs ? `${errs} 个出错` : aborted ? `${aborted} 个已终止` : "全部完成";
    const groupOpen = this.openState.has(key) ? this.openState.get(key) : live > 0 || tools.some((c) => sel?.callId === c.call_id);
    return h(
      "details",
      {
        class: `calls ${live ? "running" : errs ? "error" : aborted ? "aborted" : "done"}`,
        open: groupOpen,
        // 程序展开的（进行中、选中）标记为 auto：插入后触发的 toggle 事件不算用户的选择。
        dataset: { key, ...(groupOpen && !this.openState.has(key) ? { auto: "1" } : {}) },
      },
      h(
        "summary",
        {},
        h("span", { class: "chev" }, "›"),
        `🔧 ${tools.length} 个调用 · `,
        h("span", { class: "group-state" }, summary),
        span ? h("span", { class: "took" }, span) : null,
      ),
      h("div", { class: "call-list" }, rows),
    );
  }

  renderHeader() {
    const v = this.view;
    const first = v.payloads("run_requested")[0];
    const title = first ? (textOf(first.input) || "（非文字输入）").slice(0, 60) : "新会话";
    const run = v.activeRun();
    const attempt = run
      ? (v
          .payloads("attempt_started")
          .filter((a) => a.run_id === run)
          .at(-1)?.attempt ?? 0)
      : 0;
    fill(
      this.header,
      h(
        "div",
        { class: "chat-title" },
        h("div", { class: "chat-heading" }, title),
        h(
          "div",
          { class: "chat-subtitle" },
          h("span", { class: `dot ${run ? "busy" : "online"}` }),
          run ? "进行中" : "空闲",
          " · Session ",
          h("code", { title: v.sessionId }, short(v.sessionId)),
        ),
      ),
      h("span", { class: "spacer" }),
      attempt ? h("span", { class: "chip mono", title: `进行中的 Run ${run}` }, `Attempt A${attempt}`) : null,
      h("button", { class: "icon-btn", title: "详情抽屉", onclick: () => this.ctx.toggleDrawer?.() }, "▥"),
    );
  }

  mdCtx() {
    return {
      image: (uri, alt, cls = "") => {
        const img = h("img", { class: `art ${cls}`, alt: alt ?? "", title: alt ?? uri, src: this.ctx.artifacts.peek?.(uri) });
        this.ctx.artifacts.url(uri, this.ctx.user).then(
          (u) => (img.src = u),
          () => img.replaceWith(h("span", { class: "md-blocked" }, alt || uri)),
        );
        img.addEventListener("click", () => img.src && window.open(img.src, "_blank"));
        return img;
      },
      link: (uri) => this.ctx.artifacts.url(uri, this.ctx.user).then((u) => window.open(u, "_blank")),
    };
  }

  media(m) {
    const mime = m.mime_type ?? "";
    if (mime.startsWith("image/")) return this.mdCtx().image(m.uri, m.name, "photo");
    if (mime.startsWith("audio/")) {
      const a = h("audio", { controls: true, preload: "none", src: this.ctx.artifacts.peek?.(m.uri) });
      this.ctx.artifacts.url(m.uri, this.ctx.user).then((u) => (a.src = u));
      return h("div", { class: "voice-msg" }, "🎤 ", a);
    }
    return h(
      "a",
      { class: "file-chip", href: "#", onclick: (e) => (e.preventDefault(), this.mdCtx().link(m.uri)) },
      "📄 ",
      m.name || m.uri,
      h("small", {}, " ", bytes(m.size)),
    );
  }

  userBubble(p, e) {
    const parts = [];
    for (const b of p.input ?? []) {
      if (b.text?.text) parts.push(h("div", { class: "plain" }, b.text.text));
      else if (b.media) parts.push(this.media(b.media));
      else if (b.card_action) {
        const a = b.card_action;
        const vals = Object.entries(a.values ?? {}).map(([k, v]) => `${k}=${v}`);
        parts.push(
          h("div", { class: "action-chip" }, "👆 ", a.label || a.action, vals.length ? h("small", {}, "（", vals.join("，"), "）") : null),
        );
      } else if (b.ui_context) {
        const u = b.ui_context;
        if (u.screen || u.content || u.selection || u.ref)
          parts.push(h("div", { class: "ctx-chip", title: JSON.stringify(u, null, 2) }, "🪟 ", u.screen || "界面上下文"));
      }
    }
    const device = (p.input ?? []).find((b) => b.ui_context?.device)?.ui_context.device;
    return h(
      "div",
      { class: `user-message ${p.from_call ? "from-call" : ""}` },
      h("div", { class: "user-bubble" }, p.from_call ? h("div", { class: "tag" }, "🎙 通话中交给后台") : null, parts),
      h("div", { class: "message-origin" }, device ? `来自 ${device} · ` : "", e.steered ? "插话 · " : "", clock(e.time)),
    );
  }

  /** renderPending 展示本 Session 与进行中的委托里等待用户的审批与提问；子 Session 的审批与回答发往子 Session。 */
  renderPending() {
    const { approvals, questions } = this.view.pendingWithDelegations((sid) => this.childView(sid));
    // 输入框的文字会作为子 Agent 提问的回答时，在 placeholder 上说明（规则见 SessionView.typedAnswerTarget）。
    const target = this.answerTarget();
    this.input.placeholder = target ? `回答子 Agent：${target.question}` : this.placeholder;
    // 子 Session 在等用户时，状态行显示"等待你回复子 Agent"，而不是"等待子 Agent"。
    this.waitingUser = new Set([...approvals, ...questions].filter((x) => x.via).map((x) => x.via));
    // 列表不变时保留已有的卡片：重建会清掉正在填写的回答。
    const key = [...approvals, ...questions].map((x) => `${x.sessionId}/${x.callId}`).join(",");
    if (key === this.pendingKey) return;
    this.pendingKey = key;
    const out = [];
    for (const a of approvals) out.push(renderApproval(a, { onDecide: (ok) => this.device.decide(a.sessionId, a.callId, ok) }));
    for (const q of questions)
      out.push(
        renderQuestion(q, {
          ...this.mdCtx(),
          state: keep(this.questionState, `${q.sessionId}/${q.callId}`, () => ({ values: {}, selected: new Set(), text: "" })),
          onAnswer: (a) => this.device.answer(q.sessionId, q.callId, a),
        }),
      );
    fill(this.pending, out);
  }

  /** answerTarget 返回输入框的文字应回答的子 Agent 提问；没有时文字提交给本 Session。 */
  answerTarget() {
    return this.view.typedAnswerTarget((sid) => this.childView(sid));
  }

  /** childView 返回子 Session 的视图，第一次取到时订阅它的变化。 */
  childView(sid) {
    const cv = this.ctx.children?.(sid);
    if (cv && !this.childOffs.has(sid)) {
      const off = cv.on((kind) => kind === "event" && this.schedule("pending"));
      this.childOffs.set(sid, off);
    }
    return cv;
  }

  renderStatus() {
    const v = this.view;
    const run = v.activeRun();
    const left = [];
    if (this.device.status !== "online")
      left.push(h("span", { class: "warn" }, this.device.status === "connecting" ? "连接中…" : "设备离线"));
    if (run) {
      const waiting = new Set();
      for (const p of v.payloads("tool_call_started")) {
        if (p.run_id !== run || v.payloads("tool_result").some((r) => r.call_id === p.call_id)) continue;
        const child = p.node_id?.startsWith("@agent/") ? p.node_id.slice(7) : "";
        if (child) waiting.add(this.waitingUser.has(child) ? "你回复子 Agent" : "子 Agent");
        else waiting.add(p.node_id === "@user" ? "你的回答" : p.node_id ? "设备" : "调用");
      }
      const what = waiting.size
        ? `等待${[...waiting].join("、")}…`
        : v.draft.text
          ? "正在输出…"
          : v.draft.reasoning
            ? "正在思考…"
            : "等待模型…";
      left.push(
        h("span", { class: "waiting" }, h("span", { class: "pulse" }), what),
        h("button", { class: "kbd-btn", title: "中断进行中的 Run", onclick: () => this.interrupt() }, "⏹ 中断", h("kbd", {}, "Esc")),
      );
    }
    const others = (v.presence ?? []).filter((p) => p.device_id !== this.device.id);
    fill(
      this.status,
      h("div", { class: "row" }, left),
      others.length
        ? h(
            "span",
            { class: "watching" },
            "👀 ",
            others.map((p) => `${p.label || short(p.device_id)}${p.typing ? "（正在输入）" : ""}`).join("、"),
            " 在看",
          )
        : null,
    );
  }

  // —— 输入 ——

  composer() {
    this.placeholder = this.compact ? "输入…" : "输入消息；可粘贴或拖入图片";
    this.input = h("textarea", { rows: this.compact ? 1 : 2, placeholder: this.placeholder });
    this.input.addEventListener("keydown", (e) => {
      if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
        e.preventDefault();
        this.send();
      }
    });
    let typingAt = 0;
    this.input.addEventListener("input", () => {
      if (Date.now() - typingAt > 4000) {
        typingAt = Date.now();
        this.device.setActivity(this.view.sessionId, true, true);
      }
    });
    this.input.addEventListener("paste", (e) => {
      const files = [...(e.clipboardData?.files ?? [])];
      if (files.length) {
        e.preventDefault();
        this.addFiles(files);
      }
    });
    this.staged = h("div", { class: "staged" });
    const file = h("input", {
      type: "file",
      multiple: true,
      hidden: true,
      onchange: (e) => (this.addFiles([...e.target.files]), (e.target.value = "")),
    });
    const camera = h("input", {
      type: "file",
      accept: "image/*",
      capture: "environment",
      hidden: true,
      onchange: (e) => (this.addFiles([...e.target.files]), (e.target.value = "")),
    });
    this.micBtn = h(
      "button",
      { class: "tool voice", type: "button", title: "按住说话：松开发送语音消息（模型直接听）" },
      "🎤",
      this.compact ? null : " 按住说话",
    );
    this.micBtn.addEventListener("pointerdown", (e) => (e.preventDefault(), this.startVoice()));
    for (const ev of ["pointerup", "pointerleave"]) this.micBtn.addEventListener(ev, () => this.stopVoice());
    this.ctxToggle = h("input", { type: "checkbox" });
    this.ctxScreen = h("input", { placeholder: "界面，如 订单详情" });
    this.ctxContent = h("input", { placeholder: "可见内容（按不可信数据对待）" });
    const ctxPanel = h(
      "div",
      { class: "ctx-panel", hidden: true },
      h("label", { class: "inline" }, this.ctxToggle, " 随下一条消息附上界面上下文"),
      this.ctxScreen,
      this.ctxContent,
    );
    return h(
      "div",
      { class: "composer-wrap" },
      this.status,
      this.pending,
      h(
        "div",
        { class: "composer" },
        this.staged,
        this.input,
        h(
          "div",
          { class: "composer-tools" },
          h("button", { class: "tool", type: "button", title: "附件（图片、音频、文件）", onclick: () => file.click() }, "📎"),
          this.compact ? null : h("button", { class: "tool", type: "button", title: "拍照", onclick: () => camera.click() }, "📷"),
          this.micBtn,
          this.compact
            ? null
            : h(
                "button",
                { class: "tool", type: "button", title: "界面上下文", onclick: () => (ctxPanel.hidden = !ctxPanel.hidden) },
                "🪟",
              ),
          h("span", { class: "spacer" }),
          this.compact
            ? null
            : h(
                "button",
                { class: "tool", type: "button", title: "语音通话（全双工，需要 VOLC_SPEECH_API_KEY）", onclick: () => this.toggleCall() },
                "📞",
              ),
          (this.sendBtn = h("button", { class: "send", type: "button", title: "发送（Enter）", onclick: () => this.send() }, "发送")),
          file,
          camera,
        ),
        this.compact ? null : ctxPanel,
      ),
      this.compact ? null : h("div", { class: "composer-hint" }, "Enter 发送 · Shift+Enter 换行 · Esc 中断 · ⌘K 新建会话"),
    );
  }

  /** 上传附件：登记为上传中，发送等它们完成（send）。 */
  addFiles(files) {
    const chips = files.map((f) => {
      const chip = h(
        "span",
        { class: "staged-chip uploading" },
        f.type.startsWith("image/") ? "🖼️ " : f.type.startsWith("audio/") ? "🎤 " : "📄 ",
        f.name,
        " ",
        h("small", {}, bytes(f.size)),
      );
      this.staged.append(chip);
      return [f, chip];
    });
    const p = (async () => {
      for (const [f, chip] of chips) {
        try {
          const block = await this.device.uploadArtifact(this.view.sessionId, f.name, f.type, new Uint8Array(await f.arrayBuffer()));
          chip.classList.remove("uploading");
          if (f.type.startsWith("image/")) chip.prepend(h("img", { src: URL.createObjectURL(f), class: "thumb" }));
          this.stage(block, chip);
        } catch (e) {
          chip.remove();
          toast(`上传失败：${e.message}`, "error");
        }
      }
    })();
    this.uploads.add(p);
    p.then(() => this.uploads.delete(p));
    return p;
  }

  /** 把已上传的工件放进待发送的附件；chip 为空时按工件新建一个（发送失败后放回的语音消息）。 */
  stage(block, chip) {
    const m = block.media ?? {};
    const mime = m.mime_type ?? "";
    chip ??= h(
      "span",
      { class: "staged-chip" },
      mime.startsWith("audio/") ? "🎤 " : mime.startsWith("image/") ? "🖼️ " : "📄 ",
      m.name || m.uri,
      " ",
      h("small", {}, bytes(m.size)),
    );
    const item = { block, chip };
    this.attachments.push(item);
    chip.append(
      h(
        "button",
        { class: "x", type: "button", onclick: () => (this.attachments.splice(this.attachments.indexOf(item), 1), chip.remove()) },
        "×",
      ),
    );
    this.staged.append(chip);
  }

  /**
   * 发送输入框的内容（extra 为另外附上的内容块，如刚录好的语音消息）。去向由 routeInput 决定：子 Agent 的提问在等回答时
   * 文字发往子 Session（SessionView.typedAnswerTarget），否则提交给本 Session。发出前清空输入框，失败时把整份草稿放回。
   */
  async send(extra = []) {
    if (this.uploads.size) {
      toast("附件还在上传，上传完成后发送");
      this.sendBtn?.classList.add("waiting");
      while (this.uploads.size) await Promise.all([...this.uploads]);
      this.sendBtn?.classList.remove("waiting");
    }
    const text = this.input.value.trim();
    const items = this.attachments;
    const blocks = [...items.map((a) => a.block), ...extra];
    const ctx =
      this.ctxToggle?.checked && (this.ctxScreen.value || this.ctxContent.value)
        ? { screen: this.ctxScreen.value, content: this.ctxContent.value }
        : null;
    const route = routeInput(
      { text, attachments: blocks.length, uploading: this.uploads.size, uiContext: !!ctx },
      text && !blocks.length && !ctx ? this.answerTarget() : undefined,
    );
    if (route.kind === "empty" || route.kind === "wait") return;
    const draft = { text, items, extra, ctx };
    this.input.value = "";
    for (const a of items) a.chip.remove();
    this.attachments = [];
    if (ctx) this.ctxToggle.checked = false;
    this.device.setActivity(this.view.sessionId, true, false);
    this.scrollBottom();
    if (route.kind === "answer") return this.answerChild(route.question, draft);
    const input = [];
    if (text) input.push({ text: { text } });
    input.push(...blocks);
    if (ctx) {
      if (!text) input.push({ text: { text: "（见界面）" } });
      input.push({ ui_context: ctx });
    }
    // 原样重发上次失败的输入时沿用它的 input_id：超时或断线时它可能已经生效，服务端按 input_id 去重（duplicate）。
    // 改过的内容是新的输入，用新的 ID。
    const key = JSON.stringify(input);
    const inputId = this.failed?.key === key ? this.failed.inputId : newInputId();
    try {
      const r = await this.device.submitInput(this.view.sessionId, input, { inputId });
      if (this.failed?.key === key) this.failed = null;
      if (r.duplicate) toast("重复提交：按 input_id 去重，没有再次生效");
    } catch (e) {
      this.failed = { key, inputId };
      this.restoreDraft(draft, `发送失败：${e.message}`, "原样再发时沿用同一个 input_id，已生效的不会重复生效");
    }
  }

  /**
   * 把输入框的文字作为子 Agent 提问的回答发往子 Session。别的设备已经回答时（conflict）保留文字并提示，不自动改投本 Session：
   * 用户是在回答那个提问，改投成新输入会让父 Run 收到一句没头没尾的插话。
   */
  async answerChild(q, draft) {
    try {
      await this.device.answer(q.sessionId, q.callId, { text: draft.text });
    } catch (e) {
      const conflict = e instanceof RequestError && e.code === "conflict";
      this.restoreDraft(draft, conflict ? "这个提问已经回答过了（可能在别的设备上）" : `回答失败：${e.message}`);
    }
  }

  /**
   * 发送失败时把草稿（文字、附件、界面上下文）放回输入框。失败可能在用户已经开始写下一条之后才到达：输入框不空时不覆盖，
   * 只提示原文没有发出。
   */
  restoreDraft(d, msg, hint = "") {
    const empty = !this.input.value.trim() && !this.attachments.length && !this.uploads.size;
    if (!empty) {
      toast(`${msg}。输入框里已有新内容，没有放回；没发出的是：${d.text || "（附件）"}`, "error");
      return;
    }
    this.input.value = d.text;
    for (const a of d.items) {
      this.attachments.push(a);
      this.staged.append(a.chip);
    }
    for (const b of d.extra) this.stage(b);
    if (d.ctx) {
      this.ctxToggle.checked = true;
      this.ctxScreen.value = d.ctx.screen;
      this.ctxContent.value = d.ctx.content;
    }
    toast(`${msg}。内容已放回输入框${hint ? `，${hint}` : ""}`, "error");
  }

  async cardAction(card, action, values, e) {
    const r = await this.device.submitCardAction(this.view.sessionId, {
      card_id: card.card_id,
      call_id: card.call_id,
      action,
      values,
      seq: e.seq,
    });
    if (r.duplicate) toast("这个动作已经执行过（同一版卡片上的同一动作只生效一次）");
  }

  // —— 语音消息 ——

  async startVoice() {
    if (this.recorder) return;
    const r = (this.recorder = new Recorder());
    this.micBtn.classList.add("recording");
    try {
      // 松开按钮（stopVoice）可能早于麦克风打开：Recorder 在打开后发现已停止时释放麦克风。
      await r.start((peak) => this.micBtn.style.setProperty("--level", Math.min(1, peak * 3)));
    } catch (e) {
      if (this.recorder === r) {
        this.recorder = null;
        this.micBtn.classList.remove("recording");
      }
      toast(`无法打开麦克风：${e.message}`, "error");
    }
  }

  async stopVoice() {
    const r = this.recorder;
    if (!r) return;
    this.recorder = null;
    this.micBtn.classList.remove("recording");
    const { bytes: wav, seconds } = r.stop();
    if (seconds < 0.5) return toast("说话时间太短");
    try {
      const block = await this.device.uploadArtifact(this.view.sessionId, `voice-${Date.now()}.wav`, "audio/wav", wav);
      await this.send([block]);
    } catch (e) {
      toast(`语音消息发送失败：${e.message}`, "error");
    }
  }

  // —— 通话 ——

  async toggleCall() {
    if (this.call) return this.endCall("hangup");
    try {
      const id = this.device.startCall(this.view.sessionId);
      this.call = { id, mic: new Mic(), player: new Player(), muted: false, ready: false, user: "", assistant: "", t0: Date.now() };
      this.callBar.hidden = false;
      this.renderCall("接通中…");
    } catch (e) {
      toast(`通话失败：${e.message}`, "error");
    }
  }

  onCallEvent(ev) {
    const c = this.call;
    if (!c || ev.call_id !== c.id) return;
    if (ev.ready) {
      c.ready = true;
      c.model = ev.ready.model;
      c.rate = ev.ready.output_sample_rate || 24000;
      // 打开麦克风是异步的：通话在它完成之前结束（挂断、设备断开）时，打开后立即释放。
      c.mic
        .start((pcm) => !c.muted && this.device.sendAudio(c.id, pcm))
        .then(() => this.call !== c && c.mic.stop())
        .catch((e) => {
          if (this.call !== c) return;
          toast(`无法打开麦克风：${e.message}`, "error");
          this.endCall("hangup");
        });
      c.timer = setInterval(() => this.renderCall(), 500);
    } else if (ev.audio) c.player.play(ev.audio.pcm ?? "", ev.audio.response_id ?? "", c.rate);
    else if (ev.speech) c.player.stop();
    else if (ev.text) {
      const t = ev.text;
      if (t.role === "user") c.user = t.final ? "" : t.text;
      else c.assistant = t.final ? "" : c.assistant + (t.text ?? "");
    } else if (ev.response_done) c.assistant = "";
    else if (ev.task) c.task = `后台任务 ${short(ev.task.run_id)}：${ev.task.status}`;
    else if (ev.ended || ev.error) {
      if (ev.error) toast(`通话失败：${ev.error.code} ${ev.error.message ?? ""}`, "error");
      return this.endCall(null);
    }
    this.renderCall();
  }

  renderCall(state) {
    const c = this.call;
    if (!c) return;
    const s = Math.floor((Date.now() - c.t0) / 1000);
    fill(
      this.callBar,
      h("span", { class: "call-dot" }),
      h("b", {}, state ?? `通话中 ${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`),
      h("span", { class: "muted" }, c.model ?? ""),
      h("span", { class: "captions" }, c.user ? `你：${c.user}` : c.assistant ? `助手：${c.assistant}` : (c.task ?? "")),
      h("span", { class: "spacer" }),
      h(
        "button",
        {
          class: "tiny",
          onclick: () => ((c.muted = !c.muted), this.device.callControl(c.id, c.muted ? "MUTE" : "UNMUTE"), this.renderCall()),
        },
        c.muted ? "取消静音" : "静音",
      ),
      h("button", { class: "tiny", onclick: () => (c.player.stop(), this.device.callControl(c.id, "INTERRUPT")) }, "打断"),
      h("button", { class: "tiny danger", onclick: () => this.endCall("hangup") }, "挂断"),
    );
  }

  endCall(action) {
    const c = this.call;
    if (!c) return;
    this.call = null;
    if (action) this.device.callControl(c.id, "HANGUP");
    clearInterval(c.timer);
    c.mic.stop();
    c.player.close();
    this.callBar.hidden = true;
  }
}

/** keep 返回 m 中 key 的值，没有时以 make() 创建并保存。 */
function keep(m, key, make) {
  if (!m.has(key)) m.set(key, make());
  return m.get(key);
}
