// 与真实网关的互通测试，由 Go 端的 internal/e2e（TestWebSDK）启动网关后运行：
//
//   YANSHI_URL=ws://…/v1/connect YANSHI_TOKEN=… YANSHI_OTHER_TOKEN=… node dist/test/interop.js
//
// 网关使用 e2e 的脚本化模型："stream <n>" 逐段输出，"call ask_user <JSON>" 提问。成功时输出 "ok" 并以 0 退出。

import assert from "node:assert/strict";
import { Client, RequestError, newInputId, text, textOf, type SessionHandler } from "../src/client.js";
import type { Event } from "../src/gen/yanshi/v1/event_pb.js";
import type { LiveDelta, Presence } from "../src/gen/yanshi/v1/node_pb.js";
import { Draft, PendingQuestions } from "../src/view.js";

const url = process.env.YANSHI_URL!;
const token = process.env.YANSHI_TOKEN!;
const otherToken = process.env.YANSHI_OTHER_TOKEN!;

/** 一台设备上的一个 Session 视图。 */
class View implements SessionHandler {
  events: Event[] = [];
  draft = new Draft();
  questions = new PendingQuestions();
  presence: Presence | undefined;
  ended = "";
  // 提交之前见过的最长草稿与第一条增量。
  longestDraft = "";
  firstDelta: LiveDelta | undefined;

  onEvent(e: Event): void {
    this.events.push(e);
    this.draft.onEvent(e);
    this.questions.onEvent(e);
  }
  onDelta(d: LiveDelta): void {
    this.firstDelta ??= d;
    this.draft.onDelta(d);
    if (this.draft.text.length > this.longestDraft.length) this.longestDraft = this.draft.text;
  }
  onPresence(p: Presence): void {
    this.presence = p;
  }
  onEnded(reason: string): void {
    this.ended = reason;
  }
  completed(): number {
    return this.events.filter((e) => e.payload.case === "runCompleted").length;
  }
  lastAssistantText(): string {
    const m = this.events.findLast((e) => e.payload.case === "assistantMessage");
    return m?.payload.case === "assistantMessage" ? textOf(m.payload.value.content) : "";
  }
}

async function until(what: string, cond: () => boolean, timeoutMs = 10_000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (!cond()) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for: ${what}`);
    await new Promise((r) => setTimeout(r, 10));
  }
}

function connect(deviceId: string, tok: string): Promise<Client> {
  return new Promise((resolve) => {
    const c = new Client({ url, deviceId, token: tok, label: deviceId, kind: "browser", onConnected: () => resolve(c) });
    c.start();
  });
}

async function main(): Promise<void> {
  const a = await connect("web-a", token);
  const b = await connect("web-b", token);
  const sid = await a.createSession("dev");
  const onA = new View();
  a.subscribe(sid, 0n, onA);
  a.setActivity(sid, true);

  // 实时增量：B 在生成中途才订阅，先收到 Snapshot，拼出的草稿与提交的全文一致。
  const run = await a.submitText(sid, "stream 30");
  assert.ok(run.runId && !run.steered);
  await until("A sees deltas", () => onA.draft.text.length > 20);
  const onB = new View();
  b.subscribe(sid, 0n, onB);
  await until("both complete", () => onA.completed() === 1 && onB.completed() === 1);
  const full = Array.from({ length: 30 }, (_, i) => `chunk${i} `).join("");
  assert.equal(onA.longestDraft, full);
  assert.equal(onB.firstDelta?.snapshot, true, "late joiner starts from a snapshot");
  assert.equal(onB.longestDraft, full);
  assert.equal(onA.draft.active || onB.draft.active, false, "drafts are cleared by the commit");
  assert.deepEqual(
    onB.events.map((e) => Number(e.seq)),
    onB.events.map((_, i) => i + 1),
  );

  // 在场：B 看到 A 正把 Session 显示在前台。
  await until("presence", () => onB.presence?.viewers.some((v) => v.deviceId === "web-a" && v.focused) === true);

  // ask_user：A 的 Run 提问，两端都看到；B 点选回答，A 的问题卡片随之收起。
  const ask = JSON.stringify({
    question: "发哪份合同？",
    options: [
      { id: "a", label: "租赁合同" },
      { id: "b", label: "采购合同" },
    ],
  });
  await a.submitText(sid, `call ask_user ${ask}`, { uiContext: { screen: "合同列表" } });
  await until("both see the question", () => onA.questions.questions.length === 1 && onB.questions.questions.length === 1);
  const q = onB.questions.questions[0]!;
  assert.equal(q.question, "发哪份合同？");
  await assert.rejects(b.answer(sid, q.callId, { selected: ["zz"] }), (e) => e instanceof RequestError && e.code === "invalid");
  await b.answer(sid, q.callId, { selected: [q.options[1]!.id] });
  await until("the question closes everywhere", () => onA.questions.questions.length === 0 && onB.questions.questions.length === 0);
  await until("run completes", () => onA.completed() === 2);
  assert.match(onA.lastAssistantText(), /采购合同/);
  await assert.rejects(b.answer(sid, q.callId, { selected: ["a"] }), (e) => e instanceof RequestError && e.code === "conflict");

  // 输入去重：以同一 ID 再次提交，返回首次的结果，不再写入。
  const id = newInputId();
  const first = await a.submitInput(sid, [text("stream 1")], id);
  const again = await b.submitInput(sid, [text("stream 1")], id);
  assert.equal(again.duplicate, true);
  assert.equal(again.runId, first.runId);
  await until("run completes", () => onA.completed() === 3);
  assert.equal(onA.events.filter((e) => e.payload.case === "runRequested").length, 3);

  // 他人的 Session 一律不存在。
  const other = await connect("web-other", otherToken);
  const onOther = new View();
  other.subscribe(sid, 0n, onOther);
  await until("foreign subscription ends", () => onOther.ended !== "");
  assert.equal(onOther.ended, "not_found");
  await assert.rejects(other.closeSession(sid), (e) => e instanceof RequestError && e.code === "not_found");

  for (const c of [a, b, other]) c.stop();
}

const timer = setTimeout(() => {
  console.error("interop: timed out");
  process.exit(1);
}, 60_000);
main().then(
  () => {
    clearTimeout(timer);
    console.log("ok");
  },
  (e: unknown) => {
    console.error(e);
    process.exit(1);
  },
);
