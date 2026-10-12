import assert from "node:assert/strict";
import { test } from "node:test";
import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { EventSchema } from "../src/gen/yanshi/v1/event_pb.js";
import { LiveDeltaSchema } from "../src/gen/yanshi/v1/node_pb.js";
import { BackgroundCalls, Cards, Draft, PendingQuestions, backgroundResultOf, cardActionOf, questionOf } from "../src/view.js";
import { cardAction } from "../src/client.js";

const delta = (d: MessageInitShape<typeof LiveDeltaSchema>) => create(LiveDeltaSchema, { sessionId: "s", runId: "r1", attempt: 1, ...d });
const ev = (e: MessageInitShape<typeof EventSchema>) => create(EventSchema, { sessionId: "s", ...e });
const assistant = (seq: bigint, runId = "r1", toolCalls: { callId: string; capability: string; argumentsJson: string }[] = []) =>
  ev({ seq, payload: { case: "assistantMessage", value: { runId, attempt: 1, toolCalls } } });

test("draft: snapshot replaces, deltas append, commit clears", () => {
  const d = new Draft();
  d.onDelta(delta({ afterSeq: 3n, text: "Hel", snapshot: true }));
  d.onDelta(delta({ afterSeq: 3n, text: "lo" }));
  assert.equal(d.text, "Hello");
  assert.equal(d.runId, "r1");
  // 提交之前的事件不清除草稿。
  assert.equal(d.onEvent(assistant(3n)), false);
  assert.equal(d.onEvent(assistant(4n)), true);
  assert.equal(d.text, "");
  assert.equal(d.active, false);
  // 提交之后迟到的增量属于已提交的生成，忽略。
  assert.equal(d.onDelta(delta({ afterSeq: 3n, text: "late" })), false);
  assert.equal(d.text, "");
});

test("draft: end discards; a new generation starts afresh", () => {
  const d = new Draft();
  d.onDelta(delta({ afterSeq: 5n, text: "partial" }));
  // 其他生成的结束标记不影响当前草稿。
  assert.equal(d.onDelta(delta({ afterSeq: 5n, attempt: 2, end: true })), false);
  assert.equal(d.onDelta(delta({ afterSeq: 5n, end: true })), true);
  assert.equal(d.text, "");
  // 失败后重试的 Attempt 接在同一位置之后，是新的一条消息。
  d.onDelta(delta({ afterSeq: 5n, attempt: 2, text: "retry" }));
  d.onDelta(delta({ afterSeq: 5n, attempt: 2, text: "!" }));
  assert.equal(d.text, "retry!");
  d.onDelta(delta({ afterSeq: 5n, attempt: 2, text: "again", snapshot: true }));
  assert.equal(d.text, "again");
});

test("questions: parsed from the call, closed by the result or the end of the run", () => {
  const args = JSON.stringify({
    question: "发哪份合同？",
    options: [
      { id: "a", label: "租赁合同" },
      { id: "b", label: "采购合同" },
    ],
    fields: [{ name: "date", label: "日期", type: "date", required: true }],
  });
  const q = questionOf({ callId: "c1", capability: "ask_user", argumentsJson: args } as never);
  assert.equal(q?.question, "发哪份合同？");
  assert.equal(q?.options.length, 2);
  assert.equal(q?.multiple, false);
  assert.equal(q?.fields[0]?.type, "date");
  assert.equal(questionOf({ callId: "c2", capability: "macbook__read_file", argumentsJson: "{}" } as never), undefined);
  assert.equal(questionOf({ callId: "c3", capability: "ask_user", argumentsJson: "{" } as never), undefined);

  const p = new PendingQuestions();
  const started = (seq: bigint, runId: string, callId: string, nodeId = "@user") =>
    ev({ seq, payload: { case: "toolCallStarted", value: { runId, attempt: 1, callId, nodeId } } });
  assert.equal(p.onEvent(assistant(2n, "r1", [{ callId: "c1", capability: "ask_user", argumentsJson: args }])), false);
  // 派发给用户之前不算待回答（Worker 可能还在推进同一轮里更早的调用）。
  assert.equal(p.questions.length, 0);
  assert.equal(p.onEvent(started(3n, "r1", "c1")), true);
  p.onEvent(assistant(4n, "r2", [{ callId: "c9", capability: "ask_user", argumentsJson: args }]));
  p.onEvent(started(5n, "r2", "c9"));
  // 派发到别处的同名调用不是提问。
  p.onEvent(assistant(6n, "r2", [{ callId: "c8", capability: "ask_user", argumentsJson: args }]));
  assert.equal(p.onEvent(started(7n, "r2", "c8", "phone")), false);
  assert.deepEqual(
    p.questions.map((q) => q.callId),
    ["c1", "c9"],
  );
  assert.equal(p.onEvent(ev({ seq: 8n, payload: { case: "toolResult", value: { runId: "r1", callId: "c1" } } })), true);
  assert.equal(p.onEvent(ev({ seq: 9n, payload: { case: "runInterrupted", value: { runId: "r2" } } })), true);
  assert.equal(p.questions.length, 0);
  // Run 结束后，它尚未派发的提问也不会再出现。
  assert.equal(p.onEvent(started(10n, "r2", "c8")), false);
  assert.equal(p.questions.length, 0);
});

test("cards: show_card results appear, the same card_id replaces the earlier version", () => {
  const shown = (seq: bigint, cardId: string, callId: string, title: string) =>
    ev({
      seq,
      payload: {
        case: "toolResult",
        value: { runId: "r1", callId, content: [{ kind: { case: "card", value: { cardId, callId, title } } }] },
      },
    });
  const c = new Cards();
  assert.equal(c.onEvent(assistant(1n)), false);
  assert.equal(c.onEvent(shown(2n, "alarm", "c1", "正在设置")), true);
  assert.equal(c.onEvent(shown(3n, "weather", "c2", "天气")), true);
  assert.equal(c.onEvent(shown(5n, "alarm", "c3", "已设置 07:00")), true);
  assert.deepEqual(
    c.cards.map((v) => [v.card.cardId, v.card.title, v.seq]),
    [
      ["alarm", "已设置 07:00", 5n],
      ["weather", "天气", 3n],
    ],
  );
  const v = c.get("alarm")!;
  const b = cardAction({ cardId: v.card.cardId, callId: v.card.callId, action: "ok", seq: v.seq });
  assert.equal(b.kind?.case, "cardAction");
});

test("cards: actions are taken from new runs, steers and answers to a question", () => {
  const action = (callId: string, act: string) => ({ cardId: "alarm", callId, action: act, label: act });
  const input = (callId: string, act: string) => [
    { kind: { case: "text" as const, value: { text: "顺便" } } },
    { kind: { case: "cardAction" as const, value: action(callId, act) } },
  ];
  const requested = ev({ seq: 1n, payload: { case: "runRequested", value: { runId: "r1", input: input("c1", "ok") } } });
  const steered = ev({ seq: 2n, payload: { case: "steered", value: { runId: "r1", input: input("c1", "later") } } });
  // Run 等回答时点了卡片：动作成为提问的回答，记在 ToolResult.cardAction 中，不在任何输入里。
  const answered = ev({
    seq: 3n,
    payload: { case: "toolResult", value: { runId: "r1", callId: "q1", inputId: "card_x", cardAction: action("c3", "ok") } },
  });
  const typed = ev({ seq: 4n, payload: { case: "steered", value: { runId: "r1", input: input("c1", "ok").slice(0, 1) } } });
  assert.equal(cardActionOf(requested)?.action, "ok");
  assert.equal(cardActionOf(steered)?.action, "later");
  assert.equal(cardActionOf(answered)?.callId, "c3");
  assert.equal(cardActionOf(typed), undefined);
  assert.equal(cardActionOf(assistant(5n)), undefined);

  const c = new Cards();
  assert.equal(c.taken("c3", "ok"), false);
  for (const e of [requested, steered, answered, typed]) c.onEvent(e);
  assert.equal(c.taken("c1", "ok"), true);
  assert.equal(c.taken("c1", "later"), true);
  assert.equal(c.taken("c3", "ok"), true);
  // 键是卡片的那一版（call_id）与动作：另一版上的同一动作没有执行过。
  assert.equal(c.taken("c3", "later"), false);
  assert.equal(c.taken("c9", "ok"), false);
  // 同一动作再出现（不应发生：平台去重）不算变化。
  assert.equal(c.onEvent(answered), false);
});

test("draft: reasoning accumulates apart from the text and clears on commit", () => {
  const d = new Draft();
  d.onDelta(delta({ afterSeq: 2n, reasoning: "想" }));
  d.onDelta(delta({ afterSeq: 2n, reasoning: "一想" }));
  d.onDelta(delta({ afterSeq: 2n, text: "好" }));
  assert.equal(d.reasoning, "想一想");
  assert.equal(d.text, "好");
  d.onDelta(delta({ afterSeq: 2n, text: "好的", reasoning: "重来", snapshot: true }));
  assert.equal(d.reasoning, "重来");
  assert.equal(d.onEvent(assistant(3n)), true);
  assert.equal(d.reasoning, "");
});

test("draft: the end of the draft's Run clears it; late deltas of an ended Run are ignored", () => {
  const d = new Draft();
  d.onDelta(delta({ afterSeq: 4n, text: "说到一半" }));
  // 其他 Run 的结束不影响草稿。
  assert.equal(d.onEvent(ev({ seq: 5n, payload: { case: "runCompleted", value: { runId: "r0" } } })), false);
  assert.equal(d.text, "说到一半");
  // 断线期间生成被丢弃（没有收到 end）、Run 失败：重连后只补发终态事件。
  assert.equal(d.onEvent(ev({ seq: 6n, payload: { case: "runFailed", value: { runId: "r1" } } })), true);
  assert.equal(d.text, "");
  assert.equal(d.active, false);
  assert.equal(d.onDelta(delta({ afterSeq: 6n, text: "迟到", snapshot: true })), false);
  assert.equal(d.active, false);
  for (const k of ["runCompleted", "runInterrupted"] as const) {
    const runId = `r-${k}`;
    d.onDelta(delta({ runId, afterSeq: 7n, text: "x" }));
    assert.equal(d.onEvent(ev({ seq: 8n, payload: { case: k, value: { runId } } })), true);
    assert.equal(d.active, false);
  }
});

test("background calls: opened by a backgrounded result, settled by a result, a timeout or a cancellation", () => {
  const bg = (seq: bigint, callId: string, seconds: bigint) =>
    ev({
      seq,
      payload: {
        case: "toolResult",
        value: { runId: "r1", attempt: 1, callId, backgrounded: { nodeId: "phone", deadline: { seconds, nanos: 0 } } },
      },
    });
  const settle = (seq: bigint, kind: "runRequested" | "steered", callId: string, reason: string) =>
    ev({
      seq,
      payload: {
        case: kind,
        value: { runId: "r2", input: [{ kind: { case: "backgroundResult", value: { callId, capability: "phone__x", reason } } }] },
      },
    });
  const b = new BackgroundCalls();
  const calls = ["a", "b", "c"].map((callId) => ({ callId, capability: "phone__" + callId, argumentsJson: "{}" }));
  assert.equal(b.onEvent(assistant(1n, "r1", calls)), false);
  assert.equal(b.onEvent(bg(2n, "a", 300n)), true);
  assert.equal(b.onEvent(bg(3n, "b", 100n)), true);
  assert.equal(b.onEvent(bg(4n, "c", 200n)), true);
  assert.deepEqual(
    b.calls.map((c) => [c.callId, c.capability, c.nodeId, c.deadline]),
    [
      ["b", "phone__b", "phone", 100_000],
      ["c", "phone__c", "phone", 200_000],
      ["a", "phone__a", "phone", 300_000],
    ],
  );
  const late = settle(5n, "runRequested", "a", "result");
  assert.equal(backgroundResultOf(late)?.callId, "a");
  assert.equal(b.onEvent(late), true);
  assert.equal(b.onEvent(settle(6n, "steered", "b", "timeout")), true);
  assert.equal(b.onEvent(ev({ seq: 7n, payload: { case: "backgroundCancelled", value: { callId: "c" } } })), true);
  assert.deepEqual(b.calls, []);
  assert.equal(backgroundResultOf(ev({ seq: 8n, payload: { case: "steered", value: { runId: "r2" } } })), undefined);
});
