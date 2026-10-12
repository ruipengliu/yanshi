import assert from "node:assert/strict";
import { test } from "node:test";
import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { Client } from "../src/client.js";
import { EventSchema, type Event } from "../src/gen/yanshi/v1/event_pb.js";
import { PendingTracker, type PendingItem } from "../src/view.js";
import { FakeNetwork, type FakeSocket } from "./fake.js";

type Payload = MessageInitShape<typeof EventSchema>["payload"];

const ask = (question: string) => JSON.stringify({ question, options: [{ id: "a", label: "A" }] });

/** 一个 Session 的事件序列：seq 依次递增。 */
class Log {
  #seq = 0n;
  constructor(readonly sid: string) {}

  ev(payload: Payload): Event {
    return create(EventSchema, { sessionId: this.sid, seq: ++this.#seq, payload });
  }
  calls(runId: string, ...calls: [callId: string, capability: string, args?: string][]): Event {
    const toolCalls = calls.map(([callId, capability, argumentsJson = "{}"]) => ({ callId, capability, argumentsJson }));
    return this.ev({ case: "assistantMessage", value: { runId, attempt: 1, toolCalls } });
  }
  delegate(runId: string, callId: string, child: string): Event {
    return this.ev({ case: "toolCallStarted", value: { runId, attempt: 1, callId, nodeId: `@agent/${child}` } });
  }
  result(runId: string, callId: string): Event {
    return this.ev({ case: "toolResult", value: { runId, callId } });
  }
  approval(runId: string, callId: string, summary: string): Event {
    return this.ev({ case: "approvalRequested", value: { runId, attempt: 1, callId, summary } });
  }
  decided(runId: string, callId: string): Event {
    return this.ev({ case: "approvalDecided", value: { runId, callId, approved: true } });
  }
  completed(runId: string): Event {
    return this.ev({ case: "runCompleted", value: { runId, attempt: 1 } });
  }
}

async function setup(): Promise<{ c: Client; s: FakeSocket; t: PendingTracker; changes: PendingItem[][] }> {
  const net = new FakeNetwork();
  const c = new Client({ url: "ws://gw", deviceId: "web-1", token: "tok", webSocket: net.factory, minBackoffMs: 1, maxBackoffMs: 2 });
  c.start();
  const s = await net.connect(1);
  const changes: PendingItem[][] = [];
  const t = new PendingTracker(c, "top", { onChange: (items) => changes.push(items) });
  return { c, s, t, changes };
}

function push(s: FakeSocket, e: Event): void {
  s.push({ msg: { case: "event", value: e } });
}

/** 客户端发出的订阅与退订：["+c1@0", "-c1"]。 */
function subs(s: FakeSocket): string[] {
  return s.sent.flatMap((m) =>
    m.msg.case === "subscribe"
      ? [`+${m.msg.value.sessionId}@${m.msg.value.afterSeq}`]
      : m.msg.case === "unsubscribe"
        ? [`-${m.msg.value.sessionId}`]
        : [],
  );
}

const brief = (items: PendingItem[]) =>
  items.map((i) => `${i.kind}:${i.sessionId}/${i.callId}:${i.agent}:${i.via}:${i.kind === "question" ? i.question.question : i.summary}`);

test("pending: a delegation is followed; the child's questions and approvals carry their Session, agent and via", async () => {
  const { c, s, t, changes } = await setup();
  const top = new Log("top");
  const child = new Log("c1");
  assert.equal(t.onEvent(top.calls("r1", ["d1", "agent__helper", '{"task":"x"}'])), false);
  assert.equal(t.onEvent(top.delegate("r1", "d1", "c1")), false);
  assert.deepEqual(subs(s), ["+c1@0"], "the child Session is subscribed from the start");

  push(s, child.calls("cr1", ["q1", "ask_user", ask("去哪？")]));
  push(s, child.calls("cr1", ["w1", "phone__send_sms", "{}"]));
  push(s, child.approval("cr1", "w1", "在手机上发短信"));
  assert.deepEqual(brief(t.items), ["approval:c1/w1:helper:c1:在手机上发短信", "question:c1/q1:helper:c1:去哪？"]);
  const a = t.items[0]!;
  assert.ok(a.kind === "approval" && a.capability === "phone__send_sms" && a.runId === "cr1");
  assert.equal(changes.length, 2);

  // 喂给 onEvent 的其他 Session 的事件忽略。
  assert.equal(t.onEvent(child.result("cr1", "q1")), false);
  c.stop();
});

test("pending: grandchildren are followed too, attributed to the top-level's direct child", async () => {
  const { c, s, t } = await setup();
  const top = new Log("top");
  const child = new Log("c1");
  const grand = new Log("g1");
  t.onEvent(top.calls("r1", ["d1", "agent__planner"]));
  t.onEvent(top.delegate("r1", "d1", "c1"));
  push(s, child.calls("cr1", ["d2", "agent__helper"]));
  push(s, child.delegate("cr1", "d2", "g1"));
  assert.deepEqual(subs(s), ["+c1@0", "+g1@0"]);
  push(s, grand.calls("gr1", ["q1", "ask_user", ask("几点？")]));
  assert.deepEqual(brief(t.items), ["question:g1/q1:helper:c1:几点？"]);

  // 子 Run 结束：它的委托随之结束，孙 Session 退订、提问收起。
  push(s, child.completed("cr1"));
  assert.deepEqual(subs(s), ["+c1@0", "+g1@0", "-g1"]);
  assert.deepEqual(t.items, []);
  c.stop();
});

test("pending: the delegation's result or the end of its Run unsubscribes the child and closes its items", async () => {
  const { c, s, t, changes } = await setup();
  const top = new Log("top");
  const child = new Log("c1");
  t.onEvent(top.calls("r1", ["d1", "agent__helper"]));
  t.onEvent(top.delegate("r1", "d1", "c1"));
  push(s, child.calls("cr1", ["q1", "ask_user", ask("去哪？")]));
  assert.equal(t.items.length, 1);
  assert.equal(t.onEvent(top.result("r1", "d1")), true);
  assert.deepEqual(subs(s), ["+c1@0", "-c1"]);
  assert.deepEqual(t.items, []);
  assert.deepEqual(changes.at(-1), []);
  // 退订之后迟到的子 Session 事件不再影响列表。
  push(s, child.calls("cr1", ["q2", "ask_user", ask("还有？")]));
  assert.deepEqual(t.items, []);

  // 第二次委托：父 Run 被中断，委托随之结束。
  t.onEvent(top.calls("r2", ["d2", "agent__helper"]));
  t.onEvent(top.delegate("r2", "d2", "c2"));
  const child2 = new Log("c2");
  push(s, child2.calls("cr2", ["q3", "ask_user", ask("确定？")]));
  assert.equal(t.items.length, 1);
  assert.equal(t.onEvent(top.ev({ case: "runInterrupted", value: { runId: "r2" } })), true);
  assert.deepEqual(subs(s), ["+c1@0", "-c1", "+c2@0", "-c2"]);
  assert.deepEqual(t.items, []);

  // close 取消全部子订阅。
  t.onEvent(top.calls("r3", ["d3", "agent__helper"]));
  t.onEvent(top.delegate("r3", "d3", "c3"));
  t.close();
  assert.deepEqual(subs(s).slice(-2), ["+c3@0", "-c3"]);
  assert.equal(t.onEvent(top.calls("r3", ["q4", "ask_user", ask("?")])), false);
  c.stop();
});

test("pending: approvals close on the decision, the call's result or the end of the Run", async () => {
  const { c, t } = await setup();
  const top = new Log("top");
  t.onEvent(top.calls("r1", ["w1", "mac__write_file"], ["w2", "mac__write_file"]));
  t.onEvent(top.approval("r1", "w1", "写文件 1"));
  t.onEvent(top.approval("r1", "w2", "写文件 2"));
  assert.deepEqual(brief(t.items), ["approval:top/w1:::写文件 1", "approval:top/w2:::写文件 2"]);
  assert.equal(t.onEvent(top.decided("r1", "w1")), true);
  assert.equal(t.onEvent(top.result("r1", "w2")), true);
  assert.deepEqual(t.items, []);
  t.onEvent(top.calls("r2", ["w3", "mac__write_file"]));
  t.onEvent(top.approval("r2", "w3", "写文件 3"));
  assert.equal(t.onEvent(top.completed("r2")), true);
  assert.deepEqual(t.items, []);
  c.stop();
});

test("pending: typed text answers a sub-agent's question only when it is the one open question", async () => {
  const { c, s, t } = await setup();
  const top = new Log("top");
  const child = new Log("c1");
  // 没有提问。
  assert.equal(t.typedAnswerTarget(), undefined);

  // 恰好一个提问，在子 Session 中：选中它。
  t.onEvent(top.calls("r1", ["d1", "agent__helper"]));
  t.onEvent(top.delegate("r1", "d1", "c1"));
  push(s, child.calls("cr1", ["q1", "ask_user", ask("去哪？")]));
  assert.deepEqual(brief([t.typedAnswerTarget()!]), ["question:c1/q1:helper:c1:去哪？"]);

  // 审批不算提问。
  push(s, child.calls("cr1", ["w1", "phone__send_sms"]));
  push(s, child.approval("cr1", "w1", "发短信"));
  assert.equal(t.typedAnswerTarget()?.callId, "q1");

  // 两个待回答的提问：不选中，文字照旧提交给顶层 Session。
  push(s, child.calls("cr1", ["q2", "ask_user", ask("几点？")]));
  assert.equal(t.typedAnswerTarget(), undefined);
  push(s, child.result("cr1", "q2"));
  assert.equal(t.typedAnswerTarget()?.callId, "q1");

  // 顶层 Session 自己有提问：服务端已把文字当作它的回答，不选中子 Session 的提问。
  t.onEvent(top.calls("r1", ["q3", "ask_user", ask("要继续吗？")]));
  assert.equal(t.typedAnswerTarget(), undefined);
  t.onEvent(top.result("r1", "q3"));
  assert.equal(t.typedAnswerTarget()?.callId, "q1");

  // 只有顶层自己的提问：不选中（不在子 Session 中）。
  t.onEvent(top.result("r1", "d1"));
  t.onEvent(top.calls("r1", ["q4", "ask_user", ask("好吗？")]));
  assert.equal(t.items.length, 1);
  assert.equal(t.typedAnswerTarget(), undefined);
  c.stop();
});

test("pending: a delegation loop is not followed twice", async () => {
  const { c, s, t } = await setup();
  const top = new Log("top");
  const child = new Log("c1");
  t.onEvent(top.calls("r1", ["d1", "agent__helper"]));
  t.onEvent(top.delegate("r1", "d1", "c1"));
  push(s, child.calls("cr1", ["d2", "agent__helper"]));
  push(s, child.delegate("cr1", "d2", "top"));
  // 同一 call_id 重复派发（新的 Attempt）也只订阅一次。
  t.onEvent(top.delegate("r1", "d1", "c1"));
  assert.deepEqual(subs(s), ["+c1@0"]);
  c.stop();
});
