// 模拟设备：一条 Connection（docs/design/m3-duplex-channel.md）以 protojson 文本帧收发，可兼任 Node 与会话客户端
// （ADR-0024）。它直接实现协议而不经网页 SDK：网页 SDK 只作会话客户端，而控制台要模拟提供 Capability 的手机与电脑，
// 并让每一帧可见、可手工构造。
//
// 与 SDK 保持一致的语义（便于用控制台验证网关）：
//   - 断线后自动重连，按各订阅最后收到的 seq 续订，丢弃 seq 不大于它的事件；
//   - 带 input_id 的提交在连接断开、结果未知时以同一 ID 重试；
//   - 调用按 call_id 去重（sdk/nodesdk.Executor）："执行中"先落账、再执行；重复投递时返回已有结果；
//     执行中崩溃后再次投递，幂等能力重新执行，否则返回 outcome unknown；结果在收到 ResultAck 之前，重连后重发。
//
// 本文件不依赖 DOM，可在浏览器与 Node.js 22+（全局 WebSocket）中运行：验证场景（scenarios.js）也在 make check 中运行。

/** 连接在收到响应之前断开：结果未知。 */
export class DisconnectedError extends Error {
  constructor() {
    super("connection lost before response; outcome unknown");
    this.name = "DisconnectedError";
  }
}

/** 网关拒绝了请求；code 与 HTTP 状态码对应（invalid、not_found、conflict、rejected、quota_exceeded…）。 */
export class RequestError extends Error {
  constructor(code, message) {
    super(`${code}: ${message}`);
    this.name = "RequestError";
    this.code = code;
  }
}

export function newInputId() {
  return "in_" + randomHex(16);
}

export function randomHex(n) {
  const b = new Uint8Array(n);
  globalThis.crypto.getRandomValues(b);
  return Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
}

/** 内容块中的文本。 */
export function textOf(blocks) {
  return (blocks ?? []).map((b) => b.text?.text ?? "").join("");
}

/** Event 的载荷种类（protojson 中 oneof 的字段名，如 "run_requested"）与载荷。 */
export function payloadOf(e) {
  for (const k of Object.keys(e)) {
    if (!["id", "session_id", "seq", "time"].includes(k)) return [k, e[k]];
  }
  return ["", {}];
}

/** 执行方式：auto 按处理函数自动返回；manual 等待手工填写结果；error 返回错误；hang 不回应（观察超时）；
 *  crash 执行中"崩溃"——断开连接、留下"执行中"记录，之后自动重连（观察重新投递与去重）。 */
export const MODES = ["auto", "manual", "error", "hang", "crash"];

/**
 * 预置的 Capability。handler(args, device) 返回结果文本，抛出异常表示错误结果。
 * 设备的"文件"保存在 device.files 中，write_file 写入后 read_file 可读到。
 */
export const CAPABILITY_PRESETS = {
  read_file: {
    description: "读取设备上的文本文件",
    risk: "low",
    idempotent: true,
    schema: { type: "object", properties: { path: { type: "string" } }, required: ["path"] },
    handler: (a, d) => {
      const f = d.files[a.path ?? ""];
      if (f === undefined) throw new Error(`no such file: ${a.path ?? ""}`);
      return f;
    },
  },
  list_dir: {
    description: "列出设备上的文件",
    risk: "low",
    idempotent: true,
    schema: { type: "object", properties: {} },
    handler: (_a, d) => Object.keys(d.files).sort().join("\n"),
  },
  write_file: {
    description: "在设备上写入文本文件",
    risk: "high",
    idempotent: false,
    schema: {
      type: "object",
      properties: { path: { type: "string" }, content: { type: "string" } },
      required: ["path", "content"],
    },
    handler: (a, d) => {
      d.files[a.path ?? "untitled.txt"] = a.content ?? "";
      return `written: ${a.path ?? "untitled.txt"}`;
    },
  },
  get_location: {
    description: "获取手机的当前位置",
    risk: "low",
    idempotent: true,
    schema: { type: "object", properties: {} },
    handler: () => JSON.stringify({ city: "北京", lat: 39.9042, lng: 116.4074 }),
  },
  take_photo: {
    description: "用手机拍一张照片",
    risk: "high",
    idempotent: false,
    schema: { type: "object", properties: {} },
    handler: (_a, d) => `photo saved: IMG_${String(++d.counter).padStart(4, "0")}.jpg`,
  },
  send_sms: {
    description: "从手机发送短信",
    risk: "high",
    idempotent: false,
    schema: {
      type: "object",
      properties: { to: { type: "string" }, text: { type: "string" } },
      required: ["to", "text"],
    },
    handler: (a) => `sent to ${a.to ?? "?"}`,
  },
  ping: {
    description: "回声：返回参数",
    risk: "low",
    idempotent: true,
    schema: { type: "object", properties: {} },
    handler: (a) => `pong ${JSON.stringify(a)}`,
  },
};

/** 设备类型的预置：标签、类型、能力。 */
export const DEVICE_PRESETS = {
  phone: { label: "phone", kind: "phone", capabilities: ["get_location", "take_photo", "send_sms"] },
  desktop: { label: "macbook", kind: "desktop", capabilities: ["read_file", "list_dir", "write_file"] },
  web: { label: "web", kind: "browser", clientOnly: true, capabilities: [] },
};

/** 新建设备时的默认文件。 */
export const DEFAULT_FILES = {
  "notes.txt": "会议纪要：周四下午三点评审 M3 设计。",
  "todo.txt": "1. 回复邮件\n2. 预订会议室",
};

/** 一个 Capability 的声明与执行方式。 */
function capability(name, overrides = {}) {
  const p = CAPABILITY_PRESETS[name] ?? {
    description: "",
    risk: "low",
    idempotent: false,
    schema: { type: "object", properties: {} },
    handler: (a) => `ok ${JSON.stringify(a)}`,
  };
  return { name, mode: "auto", delayMs: 0, ...p, ...overrides };
}

/**
 * SimDevice 是一台模拟设备的一条 Connection。listener 收到的通知：
 *   {type:"status"}、{type:"frame", dir:"in"|"out", msg, raw}、{type:"event", sessionId, event}、
 *   {type:"delta", delta}、{type:"presence", presence}、{type:"ended", sessionId, reason}、{type:"ledger", entry}
 */
export class SimDevice {
  /**
   * @param {object} cfg url、deviceId、label、kind、businessLine、endUser、token、clientOnly、
   *   capabilities（名称或 {name, …覆盖}）、autoReconnect（默认 true）、files
   */
  constructor(cfg) {
    this.cfg = { autoReconnect: true, hostApp: "yanshi-console", ...cfg };
    this.id = cfg.deviceId;
    this.capabilities = (cfg.capabilities ?? []).map((c) => (typeof c === "string" ? capability(c) : capability(c.name, c)));
    this.files = { ...(cfg.files ?? DEFAULT_FILES) };
    this.counter = 0;
    /** "offline" | "connecting" | "online" */
    this.status = "offline";
    /** 网关分配的标签（Welcome.label）。 */
    this.label = "";
    this.lastError = "";
    this.connects = 0;
    /** session_id → {after, focused} */
    this.subs = new Map();
    /** call_id → 账本记录 */
    this.ledger = new Map();
    this.listeners = new Set();
    this.ws = null;
    this.pending = new Map();
    this.waiters = [];
    this.nextId = 0;
    this.wanted = false;
    this.backoff = 300;
    this.timer = null;
  }

  on(fn) {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }

  emit(n) {
    for (const fn of this.listeners) {
      try {
        fn(n);
      } catch (e) {
        console.error(e);
      }
    }
  }

  setStatus(s, err = "") {
    this.status = s;
    if (err) this.lastError = err;
    this.emit({ type: "status" });
  }

  get online() {
    return this.status === "online";
  }

  /** 连接；断开后按 autoReconnect 自动重连，直到 disconnect。 */
  connect() {
    this.wanted = true;
    clearTimeout(this.timer);
    if (this.ws) return;
    this.open();
  }

  /** 断开且不再重连。crash 为 true 时模拟进程被杀：执行中的调用留下"执行中"记录。 */
  disconnect({ crash = false } = {}) {
    this.wanted = false;
    clearTimeout(this.timer);
    this.close(crash);
  }

  /** 模拟网络中断：断开后按退避自动重连（不改变 autoReconnect 的意愿）。 */
  drop({ crash = false } = {}) {
    this.close(crash);
  }

  close(crash) {
    if (crash) this.crashExecutions();
    const ws = this.ws;
    if (ws) {
      try {
        ws.close(4000, crash ? "crash" : "client closed");
      } catch {
        // 已关闭。
      }
      this.detach(ws, crash ? "crash" : "closed");
    }
  }

  open() {
    this.setStatus("connecting");
    let ws;
    try {
      ws = new WebSocket(this.cfg.url);
    } catch (e) {
      this.setStatus("offline", String(e));
      this.scheduleReconnect();
      return;
    }
    this.ws = ws;
    let welcomed = false;
    ws.onopen = () => this.sendNow(ws, { hello: this.hello() });
    ws.onmessage = (ev) => {
      if (this.ws !== ws) return;
      let m;
      try {
        m = JSON.parse(typeof ev.data === "string" ? ev.data : new TextDecoder().decode(ev.data));
      } catch {
        this.emit({ type: "frame", dir: "in", msg: null, raw: String(ev.data) });
        return;
      }
      this.emit({ type: "frame", dir: "in", msg: m });
      if (!welcomed) {
        if (!m.welcome) {
          this.lastError = "expected welcome";
          ws.close(1002, "expected welcome");
          return;
        }
        welcomed = true;
        this.label = m.welcome.label ?? "";
        this.connects++;
        this.backoff = 300;
        this.attach(ws);
        return;
      }
      this.handle(m);
    };
    ws.onerror = () => {};
    ws.onclose = (ev) => {
      if (this.ws !== ws) return;
      const why = ev.reason ? `${ev.code} ${ev.reason}` : `closed (${ev.code})`;
      this.detach(ws, why);
    };
  }

  hello() {
    const c = this.cfg;
    return {
      node_id: c.deviceId,
      token: c.token ?? "",
      business_line: c.businessLine ?? "",
      end_user: c.endUser ?? "",
      label: c.label ?? "",
      kind: c.kind ?? "",
      host_app: c.hostApp,
      sdk_version: "console/1",
      client_only: !!c.clientOnly,
      capabilities: c.clientOnly
        ? []
        : this.capabilities.map((x) => ({
            name: x.name,
            description: x.description,
            input_schema_json: JSON.stringify(x.schema ?? { type: "object", properties: {} }),
            idempotent: !!x.idempotent,
            risk: x.risk === "high" ? "RISK_HIGH" : "RISK_LOW",
            timeout_seconds: x.timeoutSeconds ?? 0,
          })),
    };
  }

  attach(ws) {
    for (const [sid, sub] of this.subs) {
      this.sendNow(ws, { subscribe: { session_id: sid, after_seq: String(sub.after) } });
      if (sub.focused) this.sendNow(ws, { activity: { session_id: sid, focused: true } });
    }
    // 尚未被确认的结果：网关可能没有收到，重发（网关对已写入的结果幂等）。
    for (const e of this.ledger.values()) {
      if (e.state === "done" && !e.acked) this.sendResult(e);
    }
    this.setStatus("online");
    const waiters = this.waiters;
    this.waiters = [];
    for (const w of waiters) w.resolve();
  }

  detach(ws, why) {
    if (this.ws !== ws) return;
    ws.onopen = ws.onmessage = ws.onerror = ws.onclose = null;
    this.ws = null;
    const pending = [...this.pending.values()];
    this.pending.clear();
    for (const p of pending) p.reject(new DisconnectedError());
    this.setStatus("offline", why);
    this.scheduleReconnect();
  }

  scheduleReconnect() {
    if (!this.wanted || !this.cfg.autoReconnect) {
      if (!this.wanted) {
        for (const w of this.waiters) w.reject(new Error("device disconnected"));
        this.waiters = [];
      }
      return;
    }
    clearTimeout(this.timer);
    const delay = this.backoff;
    this.backoff = Math.min(this.backoff * 2, 5000);
    this.timer = setTimeout(() => {
      if (this.wanted && !this.ws) this.open();
    }, delay);
  }

  sendNow(ws, m) {
    ws.send(JSON.stringify(m));
    this.emit({ type: "frame", dir: "out", msg: m });
  }

  /** 原样发送一条 NodeMessage（protojson 对象）；未连接时抛出异常。 */
  sendRaw(m) {
    if (!this.ws || this.ws.readyState !== 1) throw new Error("not connected");
    this.sendNow(this.ws, m);
  }

  send(m) {
    if (this.online) this.sendNow(this.ws, m);
  }

  /** 等待连接建立（Welcome）。 */
  ready(timeoutMs = 10_000) {
    if (this.online) return Promise.resolve();
    return withTimeout(
      new Promise((resolve, reject) => this.waiters.push({ resolve, reject })),
      timeoutMs,
      `${this.cfg.label}: not connected`,
    );
  }

  handle(m) {
    if (m.event) {
      const e = m.event;
      const sub = this.subs.get(e.session_id);
      const seq = BigInt(e.seq ?? 0);
      // 重新订阅与旧订阅的残留可能重叠：只接受更新的事件。
      if (!sub || seq <= sub.after) return;
      sub.after = seq;
      this.emit({ type: "event", sessionId: e.session_id, event: e });
    } else if (m.delta) {
      this.emit({ type: "delta", sessionId: m.delta.session_id, delta: m.delta });
    } else if (m.presence) {
      this.emit({ type: "presence", sessionId: m.presence.session_id, presence: m.presence });
    } else if (m.subscription_ended) {
      const { session_id: sid, reason } = m.subscription_ended;
      this.subs.delete(sid);
      this.emit({ type: "ended", sessionId: sid, reason });
    } else if (m.response) {
      const r = m.response;
      const p = this.pending.get(r.request_id);
      this.pending.delete(r.request_id);
      if (!p) return;
      if (r.error) p.reject(new RequestError(r.error.code, r.error.message ?? ""));
      else p.resolve(r);
    } else if (m.invoke) {
      this.onInvoke(m.invoke);
    } else if (m.cancel) {
      const e = this.ledger.get(m.cancel.call_id);
      if (e) {
        e.cancelled = true;
        if (e.state === "executing" || e.state === "awaiting") e.state = "cancelled";
        this.emit({ type: "ledger", entry: e });
      }
    } else if (m.result_ack) {
      const e = this.ledger.get(m.result_ack.call_id);
      if (e) {
        e.acked = true;
        this.emit({ type: "ledger", entry: e });
      }
    }
  }

  // —— 会话客户端 ——

  /** 订阅 Session 中 seq 大于 after 的事件；重连后自动续订。 */
  subscribe(sessionId, after = 0n) {
    const prev = this.subs.get(sessionId);
    this.subs.set(sessionId, { after: BigInt(after), focused: prev?.focused ?? false });
    this.send({ subscribe: { session_id: sessionId, after_seq: String(after) } });
  }

  unsubscribe(sessionId) {
    this.subs.delete(sessionId);
    this.send({ unsubscribe: { session_id: sessionId } });
  }

  setActivity(sessionId, focused, typing = false) {
    const sub = this.subs.get(sessionId);
    if (!sub) return;
    sub.focused = focused;
    this.send({ activity: { session_id: sessionId, focused, typing } });
  }

  /** 发出一个 ClientRequest（op 为 {create_session: …} 之类），等待响应。 */
  async request(op, { dropAfterSend = false } = {}) {
    await this.ready();
    const request_id = `r${++this.nextId}`;
    const p = new Promise((resolve, reject) => this.pending.set(request_id, { resolve, reject }));
    this.sendNow(this.ws, { request: { request_id, ...op } });
    // 演示"请求已发出、响应没回来"：发出后立即断开，结果未知。
    if (dropAfterSend) this.drop();
    return withTimeout(p, 30_000, `${this.cfg.label}: request ${request_id} timed out`);
  }

  async createSession(agent, version = "") {
    const r = await this.request({ create_session: { agent, agent_version: version } });
    return r.session_id;
  }

  /**
   * 提交一段文本：有活跃 Run 时作为插话，Run 等待回答时作为回答，否则开启新 Run。连接在响应之前断开时，
   * 以同一 input_id 重试（inputId 为空串时不去重、不重试）。
   */
  async submit(sessionId, text, { inputId = newInputId(), uiContext, dropAfterSend = false } = {}) {
    const input = [{ text: { text } }];
    if (uiContext) input.push({ ui_context: uiContext });
    let drop = dropAfterSend;
    for (;;) {
      try {
        const r = await this.request({ submit: { session_id: sessionId, input, input_id: inputId } }, { dropAfterSend: drop });
        return {
          runId: r.run_id ?? "",
          steered: !!r.steered,
          answered: r.answered ?? "",
          duplicate: !!r.duplicate,
          inputId,
        };
      } catch (e) {
        drop = false;
        if (e instanceof DisconnectedError && inputId !== "" && this.wanted) continue;
        throw e;
      }
    }
  }

  interrupt(sessionId, runId) {
    return this.request({ interrupt: { session_id: sessionId, run_id: runId } });
  }

  decide(sessionId, callId, approve) {
    return this.request({ decide: { session_id: sessionId, call_id: callId, approve } });
  }

  answer(sessionId, callId, { selected = [], values = {}, text = "" } = {}) {
    return this.request({ answer: { session_id: sessionId, call_id: callId, selected, values, text } });
  }

  closeSession(sessionId, reason = "") {
    return this.request({ close: { session_id: sessionId, reason } });
  }

  // —— Node：执行调用 ——

  capabilityByName(name) {
    return this.capabilities.find((c) => c.name === name);
  }

  onInvoke(inv) {
    let e = this.ledger.get(inv.call_id);
    if (!e) {
      e = {
        callId: inv.call_id,
        sessionId: inv.session_id,
        runId: inv.run_id,
        capability: inv.capability,
        args: inv.arguments_json ?? "{}",
        deadline: inv.deadline ?? "",
        state: "new",
        deliveries: 0,
        executions: 0,
        result: "",
        isError: false,
        acked: false,
        cancelled: false,
        crashed: false,
      };
      this.ledger.set(e.callId, e);
    }
    e.deliveries++;
    switch (e.state) {
      case "done":
        // 重复投递：返回已有结果，不重复执行。
        this.sendResult(e);
        break;
      case "executing":
      case "awaiting":
        // 正在执行（同一进程内的重复投递）：完成时发送结果。
        break;
      case "started": {
        // 上次执行中"崩溃"：结果未知。
        const cap = this.capabilityByName(e.capability);
        if (cap?.idempotent) this.execute(e, cap, true);
        else this.finish(e, "outcome unknown: the device stopped while executing this call", true);
        break;
      }
      default:
        this.execute(e, this.capabilityByName(e.capability), false);
    }
    this.emit({ type: "ledger", entry: e });
  }

  execute(e, cap, retry) {
    if (!cap) {
      this.finish(e, `unknown capability ${e.capability}`, true);
      return;
    }
    // 先记下"执行中"再执行（sdk/nodesdk.Executor 的顺序）：崩溃后据此判断结果未知。
    e.state = "executing";
    e.executions++;
    const mode = retry && cap.mode === "crash" ? "auto" : cap.mode;
    this.emit({ type: "ledger", entry: e });
    switch (mode) {
      case "manual":
        e.state = "awaiting";
        return;
      case "hang":
        return;
      case "crash":
        if (!e.crashed) {
          e.crashed = true;
          // 执行到一半进程被杀：断开连接（"执行中"记录留下），随后自动重连，网关重新投递。
          setTimeout(() => this.drop({ crash: true }), 50);
          return;
        }
        break;
      case "error":
        setTimeout(() => this.finish(e, `simulated failure in ${cap.name}`, true), cap.delayMs ?? 0);
        return;
    }
    setTimeout(() => {
      if (e.state !== "executing") return;
      let args = {};
      try {
        args = JSON.parse(e.args || "{}");
      } catch {
        // 参数不是 JSON：按空对象处理。
      }
      try {
        this.finish(e, String(cap.handler(args, this)), false);
      } catch (err) {
        this.finish(e, err instanceof Error ? err.message : String(err), true);
      }
    }, cap.delayMs ?? 0);
  }

  /** 完成一次调用（manual 模式下由界面调用）。 */
  finish(e, text, isError) {
    if (typeof e === "string") e = this.ledger.get(e);
    if (!e || e.state === "done") return;
    e.state = "done";
    e.result = text;
    e.isError = isError;
    this.sendResult(e);
    this.emit({ type: "ledger", entry: e });
  }

  sendResult(e) {
    this.send({ result: { call_id: e.callId, content: [{ text: { text: e.result } }], is_error: e.isError } });
  }

  crashExecutions() {
    for (const e of this.ledger.values()) {
      if (e.state === "executing" || e.state === "awaiting") {
        e.state = "started";
        this.emit({ type: "ledger", entry: e });
      }
    }
  }
}

/**
 * SessionView 是一台设备上一个 Session 的视图：已提交的事件、正在生成的草稿、待审批与待回答的提问、在场设备。
 * 草稿规则同网页 SDK 的 Draft（docs/design/m3-duplex-channel.md §4）。
 */
export class SessionView {
  constructor(device, sessionId) {
    this.device = device;
    this.sessionId = sessionId;
    this.events = [];
    this.deltas = [];
    this.draft = { text: "", runId: "", attempt: 0, afterSeq: -1n };
    this.committed = 0n;
    this.presence = [];
    this.ended = "";
    this.listeners = new Set();
    this.off = device.on((n) => this.onNotice(n));
  }

  dispose() {
    this.off();
  }

  on(fn) {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }

  changed(kind) {
    for (const fn of this.listeners) fn(kind);
  }

  onNotice(n) {
    if (n.sessionId !== this.sessionId) return;
    switch (n.type) {
      case "event":
        this.events.push(n.event);
        this.onEvent(n.event);
        this.changed("event");
        break;
      case "delta":
        this.deltas.push(n.delta);
        if (this.onDelta(n.delta)) this.changed("draft");
        break;
      case "presence":
        this.presence = n.presence.viewers ?? [];
        this.changed("presence");
        break;
      case "ended":
        this.ended = n.reason;
        this.changed("ended");
        break;
    }
  }

  onDelta(d) {
    const after = BigInt(d.after_seq ?? 0);
    if (after < this.committed) return false;
    const same = d.run_id === this.draft.runId && (d.attempt ?? 0) === this.draft.attempt && after === this.draft.afterSeq;
    if (d.end) {
      if (!same) return false;
      this.clearDraft();
      return true;
    }
    if (d.snapshot || !same) {
      this.draft = { text: d.text ?? "", runId: d.run_id, attempt: d.attempt ?? 0, afterSeq: after };
      return true;
    }
    this.draft.text += d.text ?? "";
    return true;
  }

  onEvent(e) {
    const [kind] = payloadOf(e);
    if (kind !== "assistant_message") return;
    const seq = BigInt(e.seq);
    if (seq > this.committed) this.committed = seq;
    if (this.draft.runId && seq > this.draft.afterSeq) this.clearDraft();
  }

  clearDraft() {
    this.draft = { text: "", runId: "", attempt: 0, afterSeq: -1n };
  }

  /** 种类为 kind 的事件载荷（带 seq）。 */
  payloads(kind) {
    return this.events.filter((e) => payloadOf(e)[0] === kind).map((e) => ({ seq: BigInt(e.seq), ...e[kind] }));
  }

  /** 尚未结束的 Run（没有 completed、failed、interrupted）。 */
  activeRun() {
    const done = new Set(
      [...this.payloads("run_completed"), ...this.payloads("run_failed"), ...this.payloads("run_interrupted")].map((p) => p.run_id),
    );
    const runs = this.payloads("run_requested").filter((r) => !done.has(r.run_id));
    return runs.at(-1)?.run_id ?? "";
  }

  /** 待审批的调用：{callId, runId, summary, capability}。 */
  pendingApprovals() {
    const decided = new Set(this.payloads("approval_decided").map((p) => p.call_id));
    const resulted = new Set(this.payloads("tool_result").map((p) => p.call_id));
    const ended = this.endedRuns();
    return this.payloads("approval_requested")
      .filter((p) => !decided.has(p.call_id) && !resulted.has(p.call_id) && !ended.has(p.run_id))
      .map((p) => ({ callId: p.call_id, runId: p.run_id, summary: p.summary ?? "", capability: this.callCapability(p.call_id) }));
  }

  /** 待回答的 ask_user 提问：{callId, runId, question, options, multiple, fields}。 */
  pendingQuestions() {
    const resulted = new Set(this.payloads("tool_result").map((p) => p.call_id));
    const ended = this.endedRuns();
    const out = [];
    for (const m of this.payloads("assistant_message")) {
      for (const c of m.tool_calls ?? []) {
        if (c.capability !== "ask_user" || resulted.has(c.call_id) || ended.has(m.run_id)) continue;
        let a = {};
        try {
          a = JSON.parse(c.arguments_json ?? "{}");
        } catch {
          // 参数不合法的提问不会到达用户。
        }
        out.push({
          callId: c.call_id,
          runId: m.run_id,
          question: a.question ?? "",
          options: a.options ?? [],
          multiple: !!a.multiple,
          fields: a.fields ?? [],
        });
      }
    }
    return out;
  }

  endedRuns() {
    return new Set(
      [...this.payloads("run_completed"), ...this.payloads("run_failed"), ...this.payloads("run_interrupted")].map((p) => p.run_id),
    );
  }

  callCapability(callId) {
    for (const m of this.payloads("assistant_message")) {
      for (const c of m.tool_calls ?? []) if (c.call_id === callId) return c.capability;
    }
    return "";
  }

  /** Run 的终态事件（run_completed、run_failed、run_interrupted），尚未结束时为 undefined。 */
  runEnd(runId) {
    for (const k of ["run_completed", "run_failed", "run_interrupted"]) {
      const p = this.payloads(k).find((x) => x.run_id === runId);
      if (p) return { kind: k, ...p };
    }
    return undefined;
  }

  /** Run 的最后一条助手消息文本。 */
  reply(runId) {
    const m = this.payloads("assistant_message")
      .filter((x) => x.run_id === runId)
      .at(-1);
    return m ? textOf(m.content) : "";
  }

  /** 等待 pred() 为真（每次视图变化时检查）。 */
  waitFor(what, pred, timeoutMs = 10_000) {
    return waitUntil(what, pred, timeoutMs, (fn) => this.on(fn));
  }
}

/** 等待 pred() 为真：subscribe(fn) 在状态变化时调用 fn，返回取消函数。 */
export function waitUntil(what, pred, timeoutMs, subscribe) {
  return new Promise((resolve, reject) => {
    let off = () => {};
    const check = () => {
      let v;
      try {
        v = pred();
      } catch (e) {
        off();
        clearTimeout(t);
        reject(e);
        return;
      }
      if (v) {
        off();
        clearTimeout(t);
        resolve(v);
      }
    };
    const t = setTimeout(() => {
      off();
      reject(new Error(`timed out waiting for ${what}`));
    }, timeoutMs);
    off = subscribe(check);
    check();
  });
}

function withTimeout(p, ms, what) {
  let t;
  return Promise.race([
    p.finally(() => clearTimeout(t)),
    new Promise((_, reject) => {
      t = setTimeout(() => reject(new Error(what)), ms);
    }),
  ]);
}
