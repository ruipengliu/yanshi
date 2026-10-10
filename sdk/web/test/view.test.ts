import assert from "node:assert/strict";
import { test } from "node:test";
import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { EventSchema } from "../src/gen/yanshi/v1/event_pb.js";
import { LiveDeltaSchema } from "../src/gen/yanshi/v1/node_pb.js";
import { Draft, PendingQuestions, questionOf } from "../src/view.js";

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
  p.onEvent(assistant(2n, "r1", [{ callId: "c1", capability: "ask_user", argumentsJson: args }]));
  p.onEvent(assistant(3n, "r2", [{ callId: "c9", capability: "ask_user", argumentsJson: args }]));
  assert.deepEqual(
    p.questions.map((q) => q.callId),
    ["c1", "c9"],
  );
  assert.equal(p.onEvent(ev({ seq: 4n, payload: { case: "toolResult", value: { runId: "r1", callId: "c1" } } })), true);
  assert.equal(p.onEvent(ev({ seq: 5n, payload: { case: "runInterrupted", value: { runId: "r2" } } })), true);
  assert.equal(p.questions.length, 0);
});
