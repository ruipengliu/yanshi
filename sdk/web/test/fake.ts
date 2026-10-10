// 测试用的假 WebSocket：记录客户端发出的消息，由测试决定何时打开、回复与断开。

import { create, fromBinary, toBinary, type MessageInitShape } from "@bufbuild/protobuf";
import { GatewayMessageSchema, NodeMessageSchema, type NodeMessage } from "../src/gen/yanshi/v1/node_pb.js";
import type { WebSocketLike } from "../src/client.js";

type Msg = NodeMessage["msg"];
type Case = Exclude<Msg["case"], undefined>;
type ValueOf<C extends Case> = Extract<Msg, { case: C }> extends { value: infer V } ? V : never;

export class FakeSocket implements WebSocketLike {
  binaryType = "blob";
  sent: NodeMessage[] = [];
  closed = false;
  onopen: ((ev: unknown) => void) | null = null;
  onmessage: ((ev: { data: unknown }) => void) | null = null;
  onclose: ((ev: unknown) => void) | null = null;
  onerror: ((ev: unknown) => void) | null = null;
  #waiters: Array<() => void> = [];

  constructor(readonly url: string) {}

  send(data: Uint8Array): void {
    if (this.closed) throw new Error("socket closed");
    this.sent.push(fromBinary(NodeMessageSchema, data));
    for (const w of this.#waiters.splice(0)) w();
  }

  close(code = 1000): void {
    if (this.closed) return;
    this.closed = true;
    queueMicrotask(() => this.onclose?.({ code }));
  }

  open(): void {
    this.onopen?.({});
  }

  /** 网关发来一条消息。 */
  push(m: MessageInitShape<typeof GatewayMessageSchema>): void {
    const b = toBinary(GatewayMessageSchema, create(GatewayMessageSchema, m));
    this.onmessage?.({ data: b.buffer.slice(b.byteOffset, b.byteOffset + b.byteLength) });
  }

  welcome(label = "web"): void {
    this.push({ msg: { case: "welcome", value: { nodeId: "d", label } } });
  }

  /** 网关断开连接。 */
  drop(): void {
    this.close(1006);
  }

  /** 等到客户端发出满足条件的消息。 */
  async next<C extends Case>(kind: C, from = 0): Promise<ValueOf<C>> {
    for (;;) {
      const m = this.sent.slice(from).find((m) => m.msg.case === kind);
      if (m) return m.msg.value as ValueOf<C>;
      await new Promise<void>((r) => this.#waiters.push(r));
    }
  }
}

/** 记录 Client 创建的每一个假连接。 */
export class FakeNetwork {
  sockets: FakeSocket[] = [];
  #waiters: Array<() => void> = [];

  factory = (url: string): FakeSocket => {
    const s = new FakeSocket(url);
    this.sockets.push(s);
    for (const w of this.#waiters.splice(0)) w();
    return s;
  };

  /** 等到第 n 个连接（从 1 起）被创建。 */
  async socket(n: number): Promise<FakeSocket> {
    while (this.sockets.length < n) await new Promise<void>((r) => this.#waiters.push(r));
    return this.sockets[n - 1]!;
  }

  /** 等到第 n 个连接被创建，打开并完成握手。 */
  async connect(n: number): Promise<FakeSocket> {
    const s = await this.socket(n);
    s.open();
    await s.next("hello");
    s.welcome();
    return s;
  }
}
