import assert from "node:assert/strict";
import { test } from "node:test";
import { create } from "@bufbuild/protobuf";
import { Client, DisconnectedError, RequestError, StoppedError, type SessionHandler } from "../src/client.js";
import { EventSchema, type Event } from "../src/gen/yanshi/v1/event_pb.js";
import type { LiveDelta, Presence } from "../src/gen/yanshi/v1/node_pb.js";
import { FakeNetwork, type FakeSocket } from "./fake.js";

function client(net: FakeNetwork, extra: Partial<ConstructorParameters<typeof Client>[0]> = {}): Client {
  const c = new Client({
    url: "ws://gw/v1/connect",
    deviceId: "web-1",
    tokenSource: () => "tok",
    label: "Chrome",
    kind: "browser",
    webSocket: net.factory,
    minBackoffMs: 1,
    maxBackoffMs: 2,
    ...extra,
  });
  c.start();
  return c;
}

class Recorder implements SessionHandler {
  events: Event[] = [];
  deltas: LiveDelta[] = [];
  presence: Presence[] = [];
  ended = "";
  onEvent(e: Event): void {
    this.events.push(e);
  }
  onDelta(d: LiveDelta): void {
    this.deltas.push(d);
  }
  onPresence(p: Presence): void {
    this.presence.push(p);
  }
  onEnded(reason: string): void {
    this.ended = reason;
  }
}

function event(sid: string, seq: bigint): Event {
  return create(EventSchema, { sessionId: sid, seq, payload: { case: "runCompleted", value: { runId: "r1" } } });
}

function pushEvent(s: FakeSocket, sid: string, seq: bigint): void {
  s.push({ msg: { case: "event", value: event(sid, seq) } });
}

async function respond(s: FakeSocket, from: number, fill: (requestId: string) => Parameters<FakeSocket["push"]>[0]): Promise<void> {
  const req = await s.next("request", from);
  s.push(fill(req.requestId));
}

test("hello declares a client-only connection; requests wait for the welcome", async () => {
  const net = new FakeNetwork();
  const c = client(net, { pushPlatform: "webpush", pushToken: () => "pt" });
  const pending = c.createSession("assistant");
  const s = await net.socket(1);
  assert.equal(s.binaryType, "arraybuffer");
  s.open();
  const hello = await s.next("hello");
  assert.equal(hello.nodeId, "web-1");
  assert.equal(hello.token, "tok");
  assert.equal(hello.clientOnly, true);
  assert.equal(hello.pushPlatform, "webpush");
  assert.equal(hello.pushToken, "pt");
  assert.match(hello.sdkVersion, /^web\//);
  assert.equal(s.sent.length, 1, "nothing but hello before the welcome");
  s.welcome();
  const req = await s.next("request");
  assert.equal(req.op.case, "createSession");
  s.push({ msg: { case: "response", value: { requestId: req.requestId, sessionId: "s1" } } });
  assert.equal(await pending, "s1");
  c.stop();
});

test("errors carry the code and the quota reset time", async () => {
  const net = new FakeNetwork();
  const c = client(net);
  const s = await net.connect(1);
  const p = c.submitText("s1", "hi");
  await respond(s, 0, (id) => ({
    msg: {
      case: "response",
      value: { requestId: id, error: { code: "quota_exceeded", message: "daily", retryAt: { seconds: 1_800_000_000n } } },
    },
  }));
  await assert.rejects(p, (e: unknown) => {
    assert.ok(e instanceof RequestError);
    assert.equal(e.code, "quota_exceeded");
    assert.equal(e.retryAt?.getTime(), 1_800_000_000_000);
    return true;
  });
  c.stop();
});

test("reconnect resubscribes from the last seq and drops replayed events", async () => {
  const net = new FakeNetwork();
  const c = client(net);
  const rec = new Recorder();
  c.subscribe("s1", 0n, rec);
  let s = await net.connect(1);
  assert.equal((await s.next("subscribe")).afterSeq, 0n);
  c.setActivity("s1", true, true);
  pushEvent(s, "s1", 1n);
  pushEvent(s, "s1", 2n);
  pushEvent(s, "s1", 2n);
  s.drop();

  s = await net.connect(2);
  const sub = await s.next("subscribe");
  assert.equal(sub.afterSeq, 2n);
  const act = await s.next("activity");
  assert.deepEqual([act.focused, act.typing], [true, false], "focus is restored, typing is not");
  pushEvent(s, "s1", 2n);
  pushEvent(s, "s1", 3n);
  assert.deepEqual(
    rec.events.map((e) => e.seq),
    [1n, 2n, 3n],
  );
  c.stop();
});

test("a submit whose response was lost is retried with the same input ID", async () => {
  const net = new FakeNetwork();
  const c = client(net);
  let s = await net.connect(1);
  const p = c.submitText("s1", "hi", { uiContext: { screen: "订单详情", ref: "order://A1" } });
  const first = await s.next("request");
  assert.equal(first.op.case, "submit");
  assert.ok(first.op.case === "submit");
  const in1 = first.op.value;
  assert.ok(in1.inputId.startsWith("in_"));
  assert.deepEqual(
    in1.input.map((b) => b.kind.case),
    ["text", "uiContext"],
  );
  s.drop();

  s = await net.connect(2);
  const second = await s.next("request");
  const in2 = second.op.case === "submit" ? second.op.value : undefined;
  assert.equal(in2?.inputId, in1.inputId);
  s.push({ msg: { case: "response", value: { requestId: second.requestId, runId: "r1", duplicate: true } } });
  assert.deepEqual(await p, { runId: "r1", steered: false, answered: "", duplicate: true });
  c.stop();
});

test("other requests report an unknown outcome when the connection drops", async () => {
  const net = new FakeNetwork();
  const c = client(net);
  const s = await net.connect(1);
  const p = c.decide("s1", "call-1", true);
  await s.next("request");
  s.drop();
  await assert.rejects(p, DisconnectedError);
  c.stop();
});

test("stop fails pending requests and does not reconnect", async () => {
  const net = new FakeNetwork();
  const c = client(net);
  const s = await net.connect(1);
  const p = c.interrupt("s1", "r1");
  await s.next("request");
  c.stop();
  await assert.rejects(p, StoppedError);
  await assert.rejects(c.closeSession("s1"), StoppedError);
  await new Promise((r) => setTimeout(r, 20));
  assert.equal(net.sockets.length, 1);
});

test("abort signal cancels a request", async () => {
  const net = new FakeNetwork();
  const c = client(net);
  await net.connect(1);
  const ac = new AbortController();
  const p = c.createSession("assistant", "", { signal: ac.signal });
  ac.abort(new Error("gone"));
  await assert.rejects(p, /gone/);
  c.stop();
});

test("subscription end, unsubscribe, presence and a throwing handler", async () => {
  const net = new FakeNetwork();
  const uncaught: unknown[] = [];
  const c = client(net, { onHandlerError: (e) => uncaught.push(e) });
  const s = await net.connect(1);
  const rec = new Recorder();
  const thrower: SessionHandler = {
    onEvent() {
      throw new Error("handler bug");
    },
  };
  c.subscribe("s1", 0n, rec);
  const cancel = c.subscribe("s2", 0n, thrower);
  s.push({ msg: { case: "presence", value: { sessionId: "s1", viewers: [{ deviceId: "web-1", focused: true }] } } });
  pushEvent(s, "s2", 1n);
  pushEvent(s, "s1", 1n);
  s.push({ msg: { case: "subscriptionEnded", value: { sessionId: "s1", reason: "deleted" } } });
  pushEvent(s, "s1", 2n);
  cancel();
  assert.equal((await s.next("unsubscribe")).sessionId, "s2");
  assert.equal(rec.presence[0]?.viewers[0]?.deviceId, "web-1");
  assert.deepEqual(
    rec.events.map((e) => e.seq),
    [1n],
  );
  assert.equal(rec.ended, "deleted");
  assert.equal(uncaught.length, 1, "the handler's exception surfaces without breaking the connection");
  c.stop();
});

test("the token is fetched again on every connection", async () => {
  const net = new FakeNetwork();
  let n = 0;
  const c = client(net, { tokenSource: async () => `tok-${++n}` });
  let s = await net.connect(1);
  s.drop();
  s = await net.connect(2);
  assert.equal(s.sent[0]?.msg.case === "hello" && s.sent[0].msg.value.token, "tok-2");
  c.stop();
});
