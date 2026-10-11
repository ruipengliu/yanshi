// 对话组件：一台模拟设备上一个 Session 的聊天界面（"对话"视图全屏使用，"设备"视图中每台设备一个迷你版）。
// 只用公开协议：事件来自订阅（SessionView），输入、卡片动作、审批、回答、中断与通话都经这台设备的 Connection 发出，
// 附件经 HTTP API 上传为工件。多台设备打开同一个 Session 时，它们看到的是同一份事件。

import { Player, Recorder, Mic } from "./audio.js";
import { renderApproval, renderCard, renderQuestion } from "./cards.js";
import { payloadOf, textOf } from "./device.js";
import { render as markdown } from "./markdown.js";
import { bytes, clock, h, hue, short, toast, fill } from "./ui.js";

/** 工具名去掉设备标签前缀后的能力名与目标："iphone__set_alarm" → ["set_alarm", "iphone"]。 */
export function splitTool(name) {
  const i = String(name).indexOf("__");
  return i < 0 ? [name, ""] : [name.slice(i + 2), name.slice(0, i)];
}

const TOOL_ICON = {
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

export class ChatPanel {
  /**
   * @param {object} o
   *   device   SimDevice（会话客户端）
   *   view     该设备上的 SessionView
   *   ctx      {artifacts, user, children(sessionId) → SessionView|undefined, onInspect(event)}
   *   compact  迷你版：隐藏通话与部分说明
   */
  constructor({ device, view, ctx, compact = false }) {
    this.device = device;
    this.view = view;
    this.ctx = ctx;
    this.compact = compact;
    this.attachments = [];
    this.call = null;
    this.queued = false;
    this.root = h("div", { class: `chat ${compact ? "compact" : ""}` });
    this.log = h("div", { class: "chat-log" });
    this.pending = h("div", { class: "chat-pending" });
    this.status = h("div", { class: "chat-status" });
    this.callBar = h("div", { class: "call-bar", hidden: true });
    this.root.append(this.callBar, this.log, this.status, this.pending, this.composer());
    this.off = view.on(() => this.schedule());
    this.offDev = device.on((n) => {
      if (n.type === "status") this.schedule();
      if (n.type === "call") this.onCallEvent(n.event);
    });
    this.root.addEventListener("dragover", (e) => e.preventDefault());
    this.root.addEventListener("drop", (e) => {
      e.preventDefault();
      this.addFiles([...(e.dataTransfer?.files ?? [])]);
    });
    this.render();
  }

  dispose() {
    this.off();
    this.offDev();
    this.endCall("hangup");
  }

  schedule() {
    if (this.queued) return;
    this.queued = true;
    requestAnimationFrame(() => {
      this.queued = false;
      this.render();
    });
  }

  // —— 渲染 ——

  render() {
    const near = this.log.scrollHeight - this.log.scrollTop - this.log.clientHeight < 80;
    const v = this.view;
    const calls = this.callIndex();
    const cards = v.cards();
    const taken = v.cardActions();
    const versions = new Map();
    for (const e of v.events)
      for (const b of e.tool_result?.content ?? []) if (b.card) versions.set(b.card.card_id, (versions.get(b.card.card_id) ?? 0) + 1);
    const fromCall = new Set();
    const items = [];
    for (const e of v.events) {
      const [kind, p] = payloadOf(e);
      switch (kind) {
        case "run_requested":
        case "steered": {
          if (p.from_call) fromCall.add(p.run_id);
          items.push(this.userBubble(p, e));
          break;
        }
        case "assistant_message": {
          const text = textOf(p.content);
          const tools = (p.tool_calls ?? []).filter((c) => c.capability !== "ask_user");
          if (!text && !tools.length) break;
          items.push(
            h(
              "div",
              { class: "msg assistant", style: { "--run": hue(p.run_id) } },
              text
                ? h(
                    "div",
                    { class: "bubble md" },
                    fromCall.has(p.run_id) ? h("div", { class: "tag" }, "后台任务") : null,
                    markdown(text, this.mdCtx()),
                  )
                : null,
              tools.length
                ? h(
                    "div",
                    { class: "tools" },
                    tools.map((c) => this.toolChip(c, calls.get(c.call_id))),
                  )
                : null,
              this.meta(e, p),
            ),
          );
          break;
        }
        case "tool_result": {
          for (const b of p.content ?? []) {
            if (b.card) {
              const latest = cards.get(b.card.card_id);
              if (latest && latest.card.call_id !== b.card.call_id) {
                items.push(h("div", { class: "note small" }, `卡片「${b.card.title}」已更新，见下方最新版`));
                continue;
              }
              items.push(
                h(
                  "div",
                  { class: "msg assistant card-msg" },
                  renderCard(b.card, {
                    ...this.mdCtx(),
                    taken,
                    history: (versions.get(b.card.card_id) ?? 1) - 1,
                    onAction: (action, values) => this.cardAction(b.card, action, values, e),
                  }),
                ),
              );
            } else if (b.media && !p.attempt) {
              items.push(
                h(
                  "div",
                  { class: "msg assistant" },
                  h("div", { class: "bubble media" }, h("div", { class: "tag" }, "设备返回"), this.media(b.media)),
                ),
              );
            }
          }
          break;
        }
        case "call_transcript":
          items.push(
            h(
              "div",
              { class: `msg ${p.role === "assistant" ? "assistant" : "user"} voice` },
              h("div", { class: "bubble" }, "🎙 ", p.text, p.interrupted ? "……" : ""),
            ),
          );
          break;
        case "call_started":
          items.push(h("div", { class: "note" }, `📞 通话开始 · ${p.model ?? ""}`));
          break;
        case "call_ended":
          items.push(h("div", { class: "note" }, `通话结束：${p.reason ?? ""}`));
          break;
        case "run_failed":
          items.push(h("div", { class: "note error" }, `Run 失败：${p.reason ?? ""}`));
          break;
        case "run_interrupted":
          items.push(h("div", { class: "note" }, "已中断"));
          break;
        case "content_moderated":
          items.push(h("div", { class: "note error" }, `内容安全拦截（${p.stage}）`));
          break;
        case "context_compacted":
          items.push(h("div", { class: "note small" }, "上下文已压缩"));
          break;
        case "agent_switched":
          items.push(h("div", { class: "note small" }, `Agent 切换：${p.from?.version ?? ""} → ${p.to?.version ?? ""}`));
          break;
        case "memory_recalled":
          if ((p.items ?? []).length) items.push(h("div", { class: "note small" }, `🧠 召回 ${p.items.length} 条记忆`));
          break;
      }
    }
    if (v.draft.text)
      items.push(
        h(
          "div",
          { class: "msg assistant draft" },
          h("div", { class: "bubble md" }, markdown(v.draft.text, this.mdCtx()), h("span", { class: "caret" })),
        ),
      );
    fill(
      this.log,
      ...(items.length
        ? items
        : [h("div", { class: "empty" }, this.compact ? "还没有消息" : "发一条消息开始对话。可以附图片、发语音，或者打电话。")]),
    );
    if (near) this.log.scrollTop = this.log.scrollHeight;
    this.renderPending();
    this.renderStatus(calls);
  }

  mdCtx() {
    return {
      image: (uri, alt, cls = "") => {
        const img = h("img", { class: `art ${cls}`, alt: alt ?? "", title: alt ?? uri });
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
      const a = h("audio", { controls: true, preload: "none" });
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
      { class: `msg user ${p.from_call ? "from-call" : ""}`, style: { "--run": hue(p.run_id) } },
      h("div", { class: "bubble" }, p.from_call ? h("div", { class: "tag" }, "🎙 通话中交给后台") : null, parts),
      h("div", { class: "meta" }, device ? `来自 ${device} · ` : "", e.steered ? "插话 · " : "", clock(e.time)),
    );
  }

  meta(e, p) {
    const u = p.usage;
    return h(
      "div",
      { class: "meta" },
      clock(e.time),
      u ? ` · ${u.input_tokens ?? 0}+${u.output_tokens ?? 0} tok` : "",
      p.model ? ` · ${p.model}` : "",
    );
  }

  /** 每个调用的进展：call_id → {started, nodeId, result, error, approval, child}。 */
  callIndex() {
    const out = new Map();
    const get = (id) => {
      if (!out.has(id)) out.set(id, {});
      return out.get(id);
    };
    for (const e of this.view.events) {
      const [kind, p] = payloadOf(e);
      if (kind === "tool_call_started") Object.assign(get(p.call_id), { started: true, nodeId: p.node_id ?? "" });
      else if (kind === "tool_result")
        Object.assign(get(p.call_id), { done: true, error: !!p.is_error, result: textOf(p.content), blocks: p.content });
      else if (kind === "approval_requested") Object.assign(get(p.call_id), { approval: "pending", summary: p.summary });
      else if (kind === "approval_decided") Object.assign(get(p.call_id), { approval: p.approved ? "approved" : "denied" });
    }
    return out;
  }

  toolChip(c, st = {}) {
    const [cap, target] = splitTool(c.capability);
    const delegate = target === "agent";
    let state = "queued";
    if (st.approval === "pending") state = "approval";
    else if (st.approval === "denied") state = "denied";
    else if (st.done) state = st.error ? "error" : "done";
    else if (st.started) state = "running";
    const label = delegate ? `委托 ${cap}` : cap;
    const icon = delegate ? "🤝" : (TOOL_ICON[cap] ?? "🔧");
    const child = delegate && st.nodeId ? this.ctx.children?.(st.nodeId.slice("@agent/".length)) : undefined;
    const detail = h(
      "div",
      { class: "chip-detail" },
      h("div", {}, h("b", {}, "参数 "), h("code", {}, c.arguments_json)),
      st.result ? h("div", {}, h("b", {}, st.error ? "错误 " : "结果 "), h("code", {}, st.result.slice(0, 2000))) : null,
      child ? h("div", { class: "child-run" }, h("b", {}, `子 Session ${short(child.sessionId)}：`), childSummary(child)) : null,
    );
    const chip = h(
      "div",
      { class: `chip ${state}` },
      h(
        "button",
        { class: "chip-head", onclick: () => chip.classList.toggle("open") },
        h("span", { class: "chip-icon" }, icon),
        h("span", { class: "chip-name" }, label),
        target && !delegate ? h("span", { class: "chip-target" }, "@", target) : null,
        h(
          "span",
          { class: "chip-state" },
          { queued: "…", running: "执行中", approval: "待审批", denied: "已拒绝", error: "出错", done: "✓" }[state],
        ),
      ),
      detail,
    );
    return chip;
  }

  renderPending() {
    const v = this.view;
    const out = [];
    for (const a of v.pendingApprovals()) {
      out.push(renderApproval(a, { onDecide: (ok) => this.device.decide(v.sessionId, a.callId, ok) }));
    }
    for (const q of v.pendingQuestions()) {
      out.push(renderQuestion(q, { ...this.mdCtx(), onAnswer: (a) => this.device.answer(v.sessionId, q.callId, a) }));
    }
    fill(this.pending, ...out);
  }

  renderStatus(calls) {
    const v = this.view;
    const run = v.activeRun();
    const bits = [];
    if (this.device.status !== "online")
      bits.push(h("span", { class: "warn" }, this.device.status === "connecting" ? "连接中…" : "设备离线"));
    if (run) {
      const waiting = [...calls.entries()].filter(([, s]) => s.started && !s.done);
      const devices = waiting.map(([id]) => {
        const c = v.payloads("tool_call_started").find((p) => p.call_id === id);
        return c?.node_id?.startsWith("@agent/") ? "子 Agent" : c?.node_id === "@user" ? "你的回答" : "设备";
      });
      bits.push(
        h(
          "span",
          { class: "running" },
          h("i", { class: "spinner" }),
          devices.length ? `等待${[...new Set(devices)].join("、")}…` : v.draft.text ? "正在输入…" : "思考中…",
        ),
      );
      bits.push(
        h(
          "button",
          { class: "tiny", onclick: () => this.device.interrupt(v.sessionId, run).catch((e) => toast(e.message, "error")) },
          "⏹ 中断",
        ),
      );
    }
    const others = (v.presence ?? []).filter((p) => p.device_id !== this.device.id);
    if (others.length)
      bits.push(
        h(
          "span",
          { class: "presence" },
          "👀 ",
          others.map((p) => `${p.label || short(p.device_id)}${p.typing ? "（正在输入）" : ""}`).join("、"),
        ),
      );
    fill(this.status, ...bits);
  }

  // —— 输入 ——

  composer() {
    this.input = h("textarea", {
      rows: this.compact ? 1 : 2,
      placeholder: this.compact ? "输入…" : "输入消息，Enter 发送，Shift+Enter 换行；可粘贴或拖入图片",
    });
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
    this.micBtn = h("button", { class: "icon", title: "按住说话：松开发送语音消息（模型直接听）" }, "🎤");
    this.micBtn.addEventListener("pointerdown", (e) => (e.preventDefault(), this.startVoice()));
    for (const ev of ["pointerup", "pointerleave"]) this.micBtn.addEventListener(ev, () => this.stopVoice());
    this.ctxToggle = h("input", { type: "checkbox" });
    this.ctxScreen = h("input", { placeholder: "界面，如 订单详情" });
    this.ctxContent = h("input", { placeholder: "可见内容（按不可信数据对待）" });
    return h(
      "div",
      { class: "composer" },
      this.staged,
      h(
        "div",
        { class: "composer-row" },
        h("button", { class: "icon", title: "附件（图片、音频、文件）", onclick: () => file.click() }, "📎"),
        this.compact ? null : h("button", { class: "icon", title: "拍照（手机上打开相机）", onclick: () => camera.click() }, "📷"),
        this.micBtn,
        this.input,
        this.compact
          ? null
          : h("button", { class: "icon", title: "语音通话（全双工，需要 VOLC_SPEECH_API_KEY）", onclick: () => this.toggleCall() }, "📞"),
        h("button", { class: "primary send", onclick: () => this.send() }, "发送"),
        file,
        camera,
      ),
      this.compact
        ? null
        : h(
            "details",
            { class: "composer-opts" },
            h("summary", {}, "界面上下文"),
            h("label", { class: "inline" }, this.ctxToggle, " 随下一条消息附上"),
            this.ctxScreen,
            this.ctxContent,
          ),
    );
  }

  async addFiles(files) {
    for (const f of files) {
      const chip = h(
        "span",
        { class: "staged-chip uploading" },
        f.type.startsWith("image/") ? "🖼️ " : f.type.startsWith("audio/") ? "🎤 " : "📄 ",
        f.name,
        " ",
        h("small", {}, bytes(f.size)),
      );
      this.staged.append(chip);
      try {
        const block = await this.device.uploadArtifact(this.view.sessionId, f.name, f.type, new Uint8Array(await f.arrayBuffer()));
        const item = { block, chip };
        this.attachments.push(item);
        chip.classList.remove("uploading");
        if (f.type.startsWith("image/")) chip.prepend(h("img", { src: URL.createObjectURL(f), class: "thumb" }));
        chip.append(
          h("button", { class: "x", onclick: () => (this.attachments.splice(this.attachments.indexOf(item), 1), chip.remove()) }, "×"),
        );
      } catch (e) {
        chip.remove();
        toast(`上传失败：${e.message}`, "error");
      }
    }
  }

  async send(extra = []) {
    const text = this.input.value.trim();
    const blocks = [...this.attachments.map((a) => a.block), ...extra];
    if (!text && !blocks.length) return;
    const input = [];
    if (text) input.push({ text: { text } });
    input.push(...blocks);
    if (this.ctxToggle?.checked && (this.ctxScreen.value || this.ctxContent.value)) {
      if (!text) input.push({ text: { text: "（见界面）" } });
      input.push({ ui_context: { screen: this.ctxScreen.value, content: this.ctxContent.value } });
      this.ctxToggle.checked = false;
    }
    this.input.value = "";
    for (const a of this.attachments) a.chip.remove();
    this.attachments = [];
    this.device.setActivity(this.view.sessionId, true, false);
    try {
      const r = await this.device.submitInput(this.view.sessionId, input);
      if (r.duplicate) toast("重复提交：按 input_id 去重，没有再次生效");
    } catch (e) {
      toast(`发送失败：${e.message}`, "error");
      if (text) this.input.value = text;
    }
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
    this.recorder = new Recorder();
    this.micBtn.classList.add("recording");
    try {
      await this.recorder.start((peak) => this.micBtn.style.setProperty("--level", Math.min(1, peak * 3)));
    } catch (e) {
      this.recorder = null;
      this.micBtn.classList.remove("recording");
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
      c.mic
        .start((pcm) => !c.muted && this.device.sendAudio(c.id, pcm))
        .catch((e) => {
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

function childSummary(view) {
  const runs = view.payloads("run_requested");
  const last = runs.at(-1);
  if (!last) return "尚未开始";
  const end = view.runEnd(last.run_id);
  const reply = view.reply(last.run_id);
  return `${end ? { run_completed: "已完成", run_failed: "失败", run_interrupted: "已中断" }[end.kind] : "进行中"}${reply ? "：" + reply.slice(0, 200) : ""}`;
}
