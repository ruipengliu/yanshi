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
  GatewayMessageSchema,
  NodeMessageSchema,
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
  after: bigint;
  handler: SessionHandler;
  /** 最近一次 setActivity 报告的前台状态，重连后随订阅重发（"正在输入"是瞬时的，不重发）。 */
  focused: boolean;
}

interface Pending {
  resolve(r: ClientResponse): void;
  reject(e: unknown): void;
}

export class Client {
  readonly #cfg: ClientConfig;
  readonly #subs = new Map<string, Subscriber>();
  readonly #pending = new Map<string, Pending>();
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
      ws.send(encode(subscribeMsg(sid, sub.after)));
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
        const sub = this.#subs.get(e.sessionId);
        // 重新订阅与旧订阅的残留消息可能重叠：只接受比已收到的更新的事件。
        if (!sub || e.seq <= sub.after) return;
        sub.after = e.seq;
        this.#call(() => sub.handler.onEvent(e));
        return;
      }
      case "delta": {
        const d = m.msg.value;
        const sub = this.#subs.get(d.sessionId);
        if (sub?.handler.onDelta) this.#call(() => sub.handler.onDelta!(d));
        return;
      }
      case "presence": {
        const p = m.msg.value;
        const sub = this.#subs.get(p.sessionId);
        if (sub?.handler.onPresence) this.#call(() => sub.handler.onPresence!(p));
        return;
      }
      case "subscriptionEnded": {
        const { sessionId, reason } = m.msg.value;
        const sub = this.#subs.get(sessionId);
        this.#subs.delete(sessionId);
        if (sub?.handler.onEnded) this.#call(() => sub.handler.onEnded!(reason));
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
    const sub: Subscriber = { after, handler, focused: false };
    this.#subs.set(sessionId, sub);
    this.#send(subscribeMsg(sessionId, after));
    return () => {
      if (this.#subs.get(sessionId) !== sub) return;
      this.#subs.delete(sessionId);
      this.#send(create(NodeMessageSchema, { msg: { case: "unsubscribe", value: { sessionId } } }));
    };
  }

  /**
   * 报告本设备在一个已订阅 Session 中的状态：focused 表示该 Session 正显示在前台（有设备在看时不推送提醒），
   * typing 表示正在输入（网关 10 秒后视为停止，持续输入时每隔几秒重发）。未订阅的 Session 忽略。
   */
  setActivity(sessionId: string, focused: boolean, typing = false): void {
    const sub = this.#subs.get(sessionId);
    if (!sub) return;
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
