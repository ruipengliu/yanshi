// 验证场景：用模拟设备（device.js）对真实网关逐项验证多端语义。每个场景新建自己的设备与 Session，结束时断开设备；
// Session 保留，可在控制台中打开查看事件。Agent 须使用脚本化模型（agents/console.yaml，internal/model/script）。
//
// 浏览器中由控制台的"验证"页运行；make check 中由 internal/e2e 的 TestConsoleScenarios 以 Node.js 运行
// （internal/webui/testdata/run-scenarios.mjs）。不依赖 DOM。

import { DEFAULT_FILES, DEVICE_PRESETS, RequestError, SessionView, SimDevice, newInputId, textOf, waitUntil } from "./device.js";

/**
 * env：
 *   wsUrl        Connection 地址，如 ws://127.0.0.1:8080/v1/connect
 *   httpBase     HTTP API 地址，如 http://127.0.0.1:8080
 *   businessLine 业务线（-auth none 时自报；jwt 时以令牌为准，可为空）
 *   agent        使用脚本化模型的 Agent，默认 console
 *   helper       委托场景的目标 Agent（脚本化模型），默认 console-helper
 *   users        [{endUser, token}, {endUser, token}]：EndUser A 与 B（B 只用于隔离检查）
 */
export class Context {
  constructor(env, log) {
    this.env = { agent: "console", ...env };
    this.log = log;
    this.devices = [];
    this.views = [];
    this.sessions = [];
  }

  step(msg) {
    this.log(msg);
  }

  /** 环境不满足（如没有开启 serve -faults）时跳过场景。 */
  skip(msg) {
    throw new SkipError(msg);
  }

  check(cond, msg) {
    if (!cond) throw new Error(`检查失败：${msg}`);
    this.log(`✓ ${msg}`);
  }

  sleep(ms) {
    return new Promise((r) => setTimeout(r, ms));
  }

  /** 新建并连接一台设备。name 在同一 EndUser 下决定设备 ID（重复运行时复用同一 Node，标签不变）。 */
  async device(name, { preset = "web", user = 0, capabilities, ...rest } = {}) {
    const u = this.env.users[user];
    const p = DEVICE_PRESETS[preset];
    const d = new SimDevice({
      url: this.env.wsUrl,
      httpBase: this.env.httpBase,
      deviceId: `console-${u.endUser}-${name}`,
      label: `t-${name}`,
      kind: p.kind,
      businessLine: this.env.businessLine,
      endUser: u.endUser,
      token: u.token,
      clientOnly: !!p.clientOnly && !capabilities,
      capabilities: capabilities ?? p.capabilities,
      ...rest,
    });
    this.devices.push(d);
    d.connect();
    await d.ready();
    this.step(`设备 ${d.label}（${d.cfg.kind}${d.cfg.clientOnly ? "，只作会话客户端" : ""}）已连接`);
    return d;
  }

  /** 订阅 Session，返回该设备上的视图。 */
  view(device, sid, after = 0n) {
    const v = new SessionView(device, sid);
    this.views.push(v);
    device.subscribe(sid, after);
    return v;
  }

  async newSession(device) {
    const sid = await device.createSession(this.env.agent);
    this.sessions.push(sid);
    this.step(`创建 Session ${sid}`);
    return sid;
  }

  /** 等待 Run 结束，返回终态事件。 */
  runEnd(view, runId, timeoutMs = 15_000) {
    return view.waitFor(`run ${runId} to end`, () => view.runEnd(runId), timeoutMs);
  }

  /**
   * 提交调用设备能力的脚本，等到调用已派发（ToolCallStarted 或审批请求）或 Run 结束。新登记的设备至多 10 秒后
   * 出现在 Agent 的工具中（Catalog.NodeCacheTTL）：脚本回复 "no such tool" 时稍后重试。
   */
  async submitTool(device, view, text, timeoutMs = 15_000) {
    for (let i = 0; ; i++) {
      const r = await device.submit(view.sessionId, text);
      const started = () =>
        view.payloads("tool_call_started").some((p) => p.run_id === r.runId) ||
        view.payloads("approval_requested").some((p) => p.run_id === r.runId);
      await view.waitFor(`run ${r.runId} to dispatch a call`, () => started() || view.runEnd(r.runId), timeoutMs);
      if (started() || !view.reply(r.runId).startsWith("no such tool") || i >= 6) return r;
      this.step("工具尚未出现在目录缓存中，2 秒后重试");
      await this.sleep(2000);
    }
  }

  async http(method, path, { user = 0, body } = {}) {
    const headers = { "Content-Type": "application/json" };
    const token = this.env.users[user].token;
    if (token) headers.Authorization = `Bearer ${token}`;
    const res = await fetch(this.env.httpBase + path, { method, headers, body: body ? JSON.stringify(body) : undefined });
    const text = await res.text();
    let json;
    try {
      json = text ? JSON.parse(text) : undefined;
    } catch {
      json = undefined;
    }
    return { status: res.status, json };
  }

  cleanup() {
    for (const v of this.views) v.dispose();
    for (const d of this.devices) d.disconnect();
  }
}

class SkipError extends Error {}

/** 断言 f 以 RequestError(code) 失败。 */
async function rejects(c, f, code, msg) {
  try {
    await f();
  } catch (e) {
    c.check(e instanceof RequestError && e.code === code, `${msg}（${e instanceof RequestError ? e.code : e}）`);
    return;
  }
  c.check(false, `${msg}：请求没有失败`);
}

function seqs(view) {
  return view.events.map((e) => Number(e.seq));
}

function contiguous(view) {
  return seqs(view).every((s, i) => s === i + 1);
}

export const SCENARIOS = [
  {
    id: "sync",
    title: "多端实时同步",
    description: "三台设备订阅同一 Session：一台提交输入，其余设备实时收到增量与相同的已提交事件。",
    async run(c) {
      const phone = await c.device("phone", { preset: "phone" });
      const mac = await c.device("mac", { preset: "desktop" });
      const web = await c.device("web", { preset: "web" });
      const sid = await c.newSession(phone);
      const views = [c.view(phone, sid), c.view(mac, sid), c.view(web, sid)];
      const r = await phone.submit(sid, "stream 40");
      c.step(`手机提交 "stream 40"，Run ${r.runId}`);
      for (const v of views) await c.runEnd(v, r.runId);
      const ids = (v) => v.events.map((e) => e.id).join(",");
      c.check(
        views.every((v) => ids(v) === ids(views[0])),
        "三台设备收到的事件序列相同",
      );
      c.check(
        views.every((v) => contiguous(v)),
        "事件 seq 连续、无重复",
      );
      c.check(
        views.every((v) => v.deltas.some((d) => !d.end && d.text)),
        "三台设备都收到了实时增量",
      );
      c.check(
        views.every((v) => v.reply(r.runId) === "streamed" && v.draft.text === ""),
        "增量最终被提交的消息取代",
      );
      return { sessionId: sid };
    },
  },
  {
    id: "late-joiner",
    title: "晚加入者补齐草稿",
    description: "生成中途才订阅的设备，先收到到目前为止的全文（snapshot），再接着收到后续增量。",
    async run(c) {
      const phone = await c.device("phone", { preset: "phone" });
      const web = await c.device("web", { preset: "web" });
      const sid = await c.newSession(phone);
      const pv = c.view(phone, sid);
      const r = await phone.submit(sid, "stream 150");
      await pv.waitFor("draft on the phone", () => pv.draft.text.length > 30);
      c.step(`手机上的草稿已有 ${pv.draft.text.length} 字符，网页此时订阅`);
      const wv = c.view(web, sid);
      const first = await wv.waitFor("first delta on the web", () => wv.deltas[0]);
      c.check(first.snapshot === true && (first.text ?? "").startsWith("chunk0 "), "网页的第一条增量是从头开始的 snapshot");
      await c.runEnd(wv, r.runId);
      c.check(contiguous(wv) && Number(wv.events[0].seq) === 1, "网页补齐了订阅之前的全部事件");
      c.check(wv.reply(r.runId) === "streamed", "网页看到提交的回复");
      return { sessionId: sid };
    },
  },
  {
    id: "resume",
    title: "断线续传",
    description: "生成中途网络中断：设备自动重连，按最后收到的 seq 续订，事件不漏、不重。",
    async run(c) {
      const phone = await c.device("phone", { preset: "phone" });
      const web = await c.device("web", { preset: "web" });
      const sid = await c.newSession(phone);
      const pv = c.view(phone, sid);
      const wv = c.view(web, sid);
      const r = await phone.submit(sid, "stream 100");
      await pv.waitFor("draft", () => pv.draft.text.length > 0);
      const before = phone.connects;
      phone.drop();
      c.step(`手机断线（已收到 ${pv.events.length} 个事件）`);
      await c.runEnd(pv, r.runId);
      c.check(phone.connects > before, "手机自动重连");
      c.check(contiguous(pv), `手机的事件 seq 连续、无重复（${seqs(pv).join(",")}）`);
      await c.runEnd(wv, r.runId);
      c.check(pv.events.length === wv.events.length, "与未断线的网页事件数相同");
      return { sessionId: sid };
    },
  },
  {
    id: "input-dedup",
    title: "输入去重",
    description: "同一 input_id 重复提交只生效一次；请求发出后、响应之前断线，重连后以同一 ID 自动重试。",
    async run(c) {
      const phone = await c.device("phone", { preset: "phone" });
      const sid = await c.newSession(phone);
      const pv = c.view(phone, sid);
      const id = newInputId();
      const r1 = await phone.submit(sid, "你好", { inputId: id });
      await c.runEnd(pv, r1.runId);
      const r2 = await phone.submit(sid, "你好", { inputId: id });
      c.check(r2.duplicate && r2.runId === r1.runId, "重复提交返回首次的结果（duplicate）");
      const id2 = newInputId();
      const before = phone.connects;
      const r3 = await phone.submit(sid, "再来一次", { inputId: id2, dropAfterSend: true });
      c.check(phone.connects > before, "请求发出后断线，重连后重试");
      await c.runEnd(pv, r3.runId);
      const count = (x) => pv.payloads("run_requested").filter((p) => p.input_id === x).length;
      c.check(count(id) === 1 && count(id2) === 1, "每个 input_id 在日志中恰好一条 RunRequested");
      c.check(pv.reply(r3.runId) === "echo: 再来一次", "重试的输入正常执行");
      return { sessionId: sid };
    },
  },
  {
    id: "routing",
    title: "设备能力路由",
    description: "手机上提出的任务调用电脑上的 read_file：调用派发到电脑执行，结果回到 Run。",
    async run(c) {
      const mac = await c.device("mac", { preset: "desktop" });
      const phone = await c.device("phone", { preset: "phone" });
      const sid = await c.newSession(phone);
      const pv = c.view(phone, sid);
      const r = await c.submitTool(phone, pv, `call ${mac.label}__read_file {"path":"notes.txt"}`);
      await c.runEnd(pv, r.runId);
      const started = pv.payloads("tool_call_started").find((p) => p.run_id === r.runId);
      c.check(started?.node_id === mac.id, `调用派发到电脑（node_id ${started?.node_id}）`);
      const entry = mac.ledger.get(started.call_id);
      c.check(entry?.executions === 1, "电脑执行了一次");
      c.check(pv.reply(r.runId) === `done: ${DEFAULT_FILES["notes.txt"]}`, "结果回到 Run");
      const r2 = await phone.submit(sid, `call ${mac.label}__read_file {"path":"missing.txt"}`);
      await c.runEnd(pv, r2.runId);
      const res = pv.payloads("tool_result").find((p) => p.run_id === r2.runId);
      c.check(res?.is_error === true && textOf(res.content).includes("no such file"), "设备报告的错误作为错误结果交还模型");
      return { sessionId: sid };
    },
  },
  {
    id: "approval",
    title: "高风险能力的审批",
    description: "write_file 需要审批：各设备都看到审批请求，在网页上批准后电脑才执行；拒绝时不执行。",
    async run(c) {
      const mac = await c.device("mac", { preset: "desktop" });
      const phone = await c.device("phone", { preset: "phone" });
      const web = await c.device("web", { preset: "web" });
      const sid = await c.newSession(phone);
      const pv = c.view(phone, sid);
      const wv = c.view(web, sid);
      const r = await c.submitTool(phone, pv, `call ${mac.label}__write_file {"path":"plan.txt","content":"周五发布"}`);
      const [a] = await wv.waitFor("approval on the web", () => (wv.pendingApprovals().length ? wv.pendingApprovals() : null));
      await pv.waitFor("approval on the phone", () => pv.pendingApprovals().length > 0);
      c.check(!mac.ledger.has(a.callId), "批准之前电脑没有收到调用");
      await web.decide(sid, a.callId, true);
      c.step("网页批准");
      await c.runEnd(pv, r.runId);
      const decided = pv.events.find((e) => e.approval_decided?.call_id === a.callId);
      const started = pv.events.find((e) => e.tool_call_started?.call_id === a.callId);
      c.check(decided && started && BigInt(decided.seq) < BigInt(started.seq), "ApprovalDecided 在 ToolCallStarted 之前");
      c.check(mac.files["plan.txt"] === "周五发布", "电脑写入了文件");
      await rejects(c, () => phone.decide(sid, a.callId, false), "conflict", "再次审批同一调用返回 conflict");

      const r2 = await phone.submit(sid, `call ${mac.label}__write_file {"path":"evil.txt","content":"x"}`);
      const [b] = await pv.waitFor("second approval", () => (pv.pendingApprovals().length ? pv.pendingApprovals() : null));
      await phone.decide(sid, b.callId, false);
      c.step("手机拒绝");
      await c.runEnd(pv, r2.runId);
      const res = pv.payloads("tool_result").find((p) => p.call_id === b.callId);
      c.check(res?.is_error === true, "拒绝的调用以错误结果交还模型");
      c.check(!("evil.txt" in mac.files) && !mac.ledger.has(b.callId), "电脑没有执行被拒绝的调用");
      return { sessionId: sid };
    },
  },
  {
    id: "offline",
    title: "设备离线：挂起与恢复",
    description: "调用离线的电脑：Run 挂起、不占用 Worker；电脑上线后收到调用，Run 继续完成。",
    async run(c) {
      const mac = await c.device("mac", { preset: "desktop" });
      const phone = await c.device("phone", { preset: "phone" });
      mac.disconnect();
      c.step("电脑下线");
      const sid = await c.newSession(phone);
      const pv = c.view(phone, sid);
      const r = await c.submitTool(phone, pv, `call ${mac.label}__read_file {"path":"todo.txt"}`);
      await pv.waitFor("run suspended", () => pv.payloads("run_suspended").some((p) => p.run_id === r.runId));
      c.check(!pv.runEnd(r.runId), "Run 挂起等待设备");
      const st = await c.http("GET", `/v1/sessions/${sid}`);
      const w = st.json?.runs?.find((x) => x.run_id === r.runId)?.waiting;
      c.check(w?.kind === "node" && w.node_id === mac.id && w.node_online === false, "投影显示在等离线的电脑");
      mac.connect();
      c.step("电脑上线");
      await c.runEnd(pv, r.runId);
      c.check(pv.reply(r.runId) === `done: ${DEFAULT_FILES["todo.txt"]}`, "电脑上线后执行，Run 完成");
      return { sessionId: sid };
    },
  },
  {
    id: "redelivery",
    title: "执行中崩溃：重新投递与去重",
    description: "电脑执行到一半进程被杀：重连后网关重新投递同一 call_id；非幂等能力返回 outcome unknown，幂等能力重新执行。",
    async run(c) {
      const mac = await c.device("mac-crash", {
        preset: "desktop",
        capabilities: [
          { name: "sync_notes", idempotent: false, mode: "crash" },
          { name: "read_file", mode: "crash" },
        ],
      });
      const phone = await c.device("phone", { preset: "phone" });
      const sid = await c.newSession(phone);
      const pv = c.view(phone, sid);
      const r = await c.submitTool(phone, pv, `call ${mac.label}__sync_notes`);
      await c.runEnd(pv, r.runId);
      const call1 = pv.payloads("tool_call_started").find((p) => p.run_id === r.runId).call_id;
      const e1 = mac.ledger.get(call1);
      c.check(e1.deliveries === 2 && e1.executions === 1, `同一调用投递 ${e1.deliveries} 次、执行 ${e1.executions} 次`);
      c.check(pv.reply(r.runId).includes("outcome unknown"), "非幂等能力不重复执行，报告结果未知");
      const r2 = await phone.submit(sid, `call ${mac.label}__read_file {"path":"notes.txt"}`);
      await c.runEnd(pv, r2.runId);
      const call2 = pv.payloads("tool_call_started").find((p) => p.run_id === r2.runId).call_id;
      const e2 = mac.ledger.get(call2);
      c.check(e2.deliveries === 2 && e2.executions === 2, "幂等能力在重新投递后重新执行");
      c.check(pv.reply(r2.runId) === `done: ${DEFAULT_FILES["notes.txt"]}`, "重新执行的结果回到 Run");
      return { sessionId: sid };
    },
  },
  {
    id: "ask-user",
    title: "提问由另一台设备回答",
    description: "Agent 经 ask_user 提问：手机上发起，在网页上点选回答；也可以直接输入文字作为回答。",
    async run(c) {
      const phone = await c.device("phone", { preset: "phone" });
      const web = await c.device("web", { preset: "web" });
      const sid = await c.newSession(phone);
      const pv = c.view(phone, sid);
      const wv = c.view(web, sid);
      const r = await phone.submit(sid, "ask 周会改到哪天？| 周四 | 周五");
      const [q] = await wv.waitFor("question on the web", () => (wv.pendingQuestions().length ? wv.pendingQuestions() : null));
      c.check(q.question === "周会改到哪天？" && q.options.length === 2, "网页看到问题与选项");
      await web.answer(sid, q.callId, { selected: ["2"] });
      c.step("网页选择 周五");
      await c.runEnd(pv, r.runId);
      c.check(pv.pendingQuestions().length === 0, "手机上的提问随之收起");
      const res = pv.payloads("tool_result").find((p) => p.call_id === q.callId);
      c.check(res && !res.is_error, "回答作为调用结果回到 Run");
      await rejects(c, () => phone.answer(sid, q.callId, { selected: ["1"] }), "conflict", "已回答的提问再次回答返回 conflict");

      const r2 = await phone.submit(sid, "ask 还有别的安排吗？");
      const [q2] = await pv.waitFor("second question", () => (pv.pendingQuestions().length ? pv.pendingQuestions() : null));
      const typed = await web.submit(sid, "没有了");
      c.check(typed.answered === q2.callId, "Run 等待回答时，输入的文字作为回答");
      await c.runEnd(pv, r2.runId);
      return { sessionId: sid };
    },
  },
  {
    id: "steer-interrupt",
    title: "插话与中断",
    description: "Run 进行中另一台设备插话（并入当前 Run），随后中断；中断后 Session 继续接收新输入。",
    async run(c) {
      const phone = await c.device("phone", { preset: "phone" });
      const web = await c.device("web", { preset: "web" });
      const sid = await c.newSession(phone);
      const pv = c.view(phone, sid);
      const wv = c.view(web, sid);
      const r = await phone.submit(sid, "stream 400");
      await pv.waitFor("draft", () => pv.draft.text.length > 0);
      const s = await web.submit(sid, "顺便查一下天气");
      c.check(s.steered && s.runId === r.runId, "网页的输入作为插话并入进行中的 Run");
      await wv.waitFor("steered event", () => wv.payloads("steered").some((p) => p.run_id === r.runId));
      await web.interrupt(sid, r.runId);
      const end = await c.runEnd(pv, r.runId);
      c.check(end.kind === "run_interrupted", "Run 被中断");
      const r2 = await phone.submit(sid, "你好");
      c.check(!r2.steered && r2.runId !== r.runId, "中断后的输入开启新 Run");
      await c.runEnd(pv, r2.runId);
      c.check(pv.reply(r2.runId) === "echo: 你好", "新 Run 正常完成");
      return { sessionId: sid };
    },
  },
  {
    id: "presence",
    title: "在场与正在输入",
    description: "订阅同一 Session 的设备互相看到对方：谁把它显示在前台、谁正在输入。",
    async run(c) {
      const phone = await c.device("phone", { preset: "phone" });
      const web = await c.device("web", { preset: "web" });
      const sid = await c.newSession(phone);
      const pv = c.view(phone, sid);
      const wv = c.view(web, sid);
      phone.setActivity(sid, true);
      web.setActivity(sid, false, true);
      const viewer = (v, d) => v.presence.find((x) => x.device_id === d.id);
      await pv.waitFor("web typing on the phone", () => viewer(pv, web)?.typing);
      c.check(true, "手机看到网页正在输入");
      await wv.waitFor("phone focused on the web", () => viewer(wv, phone)?.focused);
      c.check(true, "网页看到手机把 Session 显示在前台");
      web.setActivity(sid, false, false);
      await pv.waitFor("web stopped typing", () => viewer(pv, web) && !viewer(pv, web).typing);
      c.check(true, "停止输入后随之更新");
      web.unsubscribe(sid);
      await pv.waitFor("web left", () => !viewer(pv, web));
      c.check(true, "取消订阅的设备从在场列表中消失");
      return { sessionId: sid };
    },
  },
  {
    id: "isolation",
    title: "他人的 Session 不可见",
    description: "另一个 EndUser 的设备订阅、提交、审批都按不存在处理（not_found），不泄露 Session 是否存在。",
    async run(c) {
      const phone = await c.device("phone", { preset: "phone" });
      const other = await c.device("other", { preset: "web", user: 1 });
      const sid = await c.newSession(phone);
      const ov = c.view(other, sid);
      await ov.waitFor("subscription ended", () => ov.ended);
      c.check(ov.ended === "not_found" && ov.events.length === 0, "订阅以 not_found 结束，没有收到任何事件");
      await rejects(c, () => other.submit(sid, "hi"), "not_found", "提交输入返回 not_found");
      await rejects(c, () => other.interrupt(sid, "run_x"), "not_found", "中断返回 not_found");
      await rejects(c, () => other.closeSession(sid), "not_found", "关闭返回 not_found");
      if (c.env.users[1].token) {
        const st = await c.http("GET", `/v1/sessions/${sid}`, { user: 1 });
        c.check(st.status === 404, "HTTP API 同样返回 404");
      } else {
        c.step("-auth none 下 HTTP API 不校验身份，跳过 HTTP 检查");
      }
      return { sessionId: sid };
    },
  },
  {
    id: "reasoning",
    title: "思考过程",
    description: "思考过程以单独的增量推送（与正文分开），生成中途订阅的设备在 snapshot 中补齐它，提交后写入 AssistantMessage.reasoning，并带上耗时与 token（ADR-0034）。",
    async run(c) {
      const phone = await c.device("phone", { preset: "web" });
      const web = await c.device("web", { preset: "web" });
      const sid = await c.newSession(phone);
      const pv = c.view(phone, sid);
      const r = await phone.submit(sid, "think 60\n你好");
      await pv.waitFor("reasoning deltas", () => pv.draft.reasoning.length > 40);
      c.check(pv.draft.text === "" && pv.draft.reasoning.startsWith("思考第 1 步。"), "思考过程以单独的增量推送，正文尚未开始");
      const wv = c.view(web, sid);
      const first = await wv.waitFor("first delta on the web", () => wv.deltas[0]);
      c.check(first.snapshot === true && (first.reasoning ?? "").startsWith("思考第 1 步。"), "生成中途订阅的设备在 snapshot 中补齐思考过程");
      await c.runEnd(wv, r.runId, 20_000);
      const m = wv.payloads("assistant_message").find((p) => p.run_id === r.runId);
      c.check((m.reasoning ?? "").includes("思考第 60 步。") && !textOf(m.content).includes("思考"), "提交后思考过程写入 reasoning，与正文分开");
      c.check(Number(m.usage?.reasoning_tokens ?? 0) > 0 && m.finish_reason === "stop" && m.duration_ms > 0, "记录了思考 token、结束原因与耗时");
      c.check(wv.draft.reasoning === "" && wv.draft.text === "", "提交后草稿（含思考过程）清空");
      return { sessionId: sid };
    },
  },
  {
    id: "cards",
    title: "卡片",
    description:
      "show_card 展示的卡片多端可见；同一 card_id 再次展示即替换；点按钮作为一条输入，标签由平台按卡片填写；两台设备连点只生效一次；不存在的动作被拒绝。",
    async run(c) {
      const phone = await c.device("phone", { preset: "phone" });
      const web = await c.device("web", { preset: "web" });
      const sid = await c.newSession(web);
      const pv = c.view(phone, sid);
      const wv = c.view(web, sid);
      const show = (title, buttons) =>
        `call show_card ${JSON.stringify({ card_id: "alarm", title, image: "icon:alarm", body: [{ type: "facts", facts: [{ label: "设备", value: "手机" }] }], buttons })}`;
      let r = await web.submit(
        sid,
        show("闹钟 07:00", [
          { id: "ok", label: "好的" },
          { id: "later", label: "改到 7:30" },
        ]),
      );
      await c.runEnd(wv, r.runId);
      const first = await pv.waitFor("card on the phone", () => pv.cards().get("alarm"));
      c.check(first.card.title === "闹钟 07:00" && first.card.buttons.length === 2, "手机上看到网页那边展示的卡片");
      r = await web.submit(sid, show("闹钟 07:30", [{ id: "ok", label: "好的" }]));
      await c.runEnd(wv, r.runId);
      const latest = await pv.waitFor(
        "replaced card",
        () => pv.cards().get("alarm")?.card.title === "闹钟 07:30" && pv.cards().get("alarm"),
      );
      const versions = pv.events.filter((e) => e.tool_result?.content?.some((b) => b.card?.card_id === "alarm")).length;
      c.check(latest.card.call_id !== first.card.call_id && versions === 2, "同一 card_id 再次展示替换了前一版（日志中两版都在）");
      const action = { card_id: "alarm", call_id: latest.card.call_id, action: "ok", seq: latest.seq };
      const a = await phone.submitCardAction(sid, action);
      c.check(!a.duplicate && !a.steered, '在手机上点"好的"：开启新 Run');
      await c.runEnd(pv, a.runId);
      const input = pv
        .payloads("run_requested")
        .find((p) => p.run_id === a.runId)
        .input.find((b) => b.card_action).card_action;
      c.check(input.label === "好的", "动作的标签由平台按卡片填写");
      c.check(pv.reply(a.runId).includes("选择了「好的」"), "模型（脚本）看到的是卡片动作的文字");
      const again = await web.submitCardAction(sid, action);
      c.check(again.duplicate && again.runId === a.runId, "网页上再点一次同一个按钮：duplicate，不再生效");
      await rejects(c, () => web.submitCardAction(sid, { ...action, action: "nope" }), "invalid", "卡片上没有的动作被拒绝");
      return { sessionId: sid };
    },
  },
  {
    id: "delegation",
    title: "委托",
    description:
      "委托是路由到子 Session 的调用（ADR-0031）：父 Run 挂起，子 Session 完成后由它的 Worker 写回结果，父 Run 接着完成；子 Session 可以订阅查看。",
    async run(c) {
      const web = await c.device("web", { preset: "web" });
      const sid = await c.newSession(web);
      const v = c.view(web, sid);
      const helper = c.env.helper ?? "console-helper";
      const r = await web.submit(sid, `call agent__${helper} {"task":"子任务：复述这句话"}`);
      c.step(`委托给 ${helper}，Run ${r.runId}`);
      const end = await c.runEnd(v, r.runId, 30_000);
      c.check(end.kind === "run_completed", "父 Run 完成");
      const d = v.delegations()[0];
      c.check(d && d.childSessionId, `调用派发到子 Session（node ${d ? "@agent/" + d.childSessionId : "?"}）`);
      const cv = c.view(web, d.childSessionId);
      const child = await cv.waitFor("child run", () => cv.payloads("run_requested")[0]);
      c.check(textOf(child.input).includes("子任务"), "子 Session 的输入是委托的任务");
      await c.runEnd(cv, child.run_id);
      c.check(v.reply(r.runId).includes("子任务"), "子 Run 的回复作为调用结果写回父 Run");
      return { sessionId: sid };
    },
  },
  {
    id: "delegation-ask",
    title: "子 Agent 提问",
    description:
      "子 Run 的 ask_user 路由到用户本人，写在子 Session 里：父 Session 的界面须一并展示（pendingWithDelegations），回答发往子 Session，父 Run 随委托完成。",
    async run(c) {
      const web = await c.device("web", { preset: "web" });
      const sid = await c.newSession(web);
      const v = c.view(web, sid);
      const views = new Map();
      const childView = (s) => views.get(s) ?? (views.set(s, c.view(web, s)), views.get(s));
      const helper = c.env.helper ?? "console-helper";
      const r = await web.submit(sid, `call agent__${helper} {"task":"ask 去哪个城市？| 北京 | 上海"}`);
      // 和对话面板一样：订阅委托出去的子 Session，它的变化才会让父 Session 的待回答列表更新。
      const d = await v.waitFor("delegation", () => v.delegations()[0]);
      const cv = childView(d.childSessionId);
      const [q] = await waitUntil(
        "child question",
        () => {
          const { questions } = v.pendingWithDelegations(childView);
          return questions.length ? questions : null;
        },
        10_000,
        (fn) => {
          const offs = [v.on(fn), cv.on(fn)];
          return () => offs.forEach((off) => off());
        },
      );
      c.check(v.pendingQuestions().length === 0 && q.sessionId !== sid, "提问写在子 Session 里，父 Session 自身没有待回答的提问");
      c.check(q.question === "去哪个城市？" && q.agent === helper && q.via === q.sessionId, "父 Session 的界面看到子 Agent 的提问");
      await web.answer(q.sessionId, q.callId, { selected: ["2"] });
      c.step("回答发往子 Session：上海");
      const end = await c.runEnd(v, r.runId, 30_000);
      c.check(end.kind === "run_completed" && v.reply(r.runId).includes("上海"), "回答经子 Run 写回，父 Run 完成");
      c.check(v.pendingWithDelegations(childView).questions.length === 0, "提问随之收起");
      return { sessionId: sid };
    },
  },
  {
    id: "delegation-typed-answer",
    title: "文字回答子 Agent",
    description:
      "子 Agent 的提问是唯一待回答的提问时，输入框的文字作为它的回答发往子 Session（typedAnswerTarget），父 Run 随委托完成；" +
      "已回答后再发为 conflict（对话中保留文字、提示，不改投父 Session）。父 Session 自己有提问时，文字照旧提交给父 Session。",
    async run(c) {
      const web = await c.device("web", { preset: "web" });
      const sid = await c.newSession(web);
      const v = c.view(web, sid);
      const views = new Map();
      const childView = (s) => views.get(s) ?? (views.set(s, c.view(web, s)), views.get(s));
      const helper = c.env.helper ?? "console-helper";
      const r = await web.submit(sid, `call agent__${helper} {"task":"ask 去哪个城市？| 北京 | 上海"}`);
      const d = await v.waitFor("delegation", () => v.delegations()[0]);
      const cv = childView(d.childSessionId);
      const q = await waitUntil(
        "typed answer target",
        () => v.typedAnswerTarget(childView),
        10_000,
        (fn) => {
          const offs = [v.on(fn), cv.on(fn)];
          return () => offs.forEach((off) => off());
        },
      );
      c.check(
        q.question === "去哪个城市？" && q.sessionId === d.childSessionId && q.via === q.sessionId,
        "输入框的文字选中子 Agent 的提问",
      );
      // 规则本身：两个待回答的提问时不选中。网关上同一时刻只有一条委托链在推进（扇出至多为 1，调用按顺序进行），
      // 脚本化模型造不出两个，这里在子 Session 的视图上多放一个提问来检查规则。
      const twice = (s) => {
        const real = childView(s);
        return Object.assign(Object.create(real), { pendingQuestions: () => [...real.pendingQuestions(), { ...q, callId: "second" }] });
      };
      c.check(v.typedAnswerTarget(twice) === undefined, "两个待回答的提问时不选中，文字照旧提交给父 Session");
      await web.answer(q.sessionId, q.callId, { text: "上海" });
      c.step("文字以 answer 发往子 Session：上海");
      const end = await c.runEnd(v, r.runId, 30_000);
      c.check(end.kind === "run_completed" && v.reply(r.runId).includes("上海"), "回答经子 Run 写回，父 Run 完成");
      c.check(v.typedAnswerTarget(childView) === undefined, "提问收起后不再选中");
      await rejects(c, () => web.answer(q.sessionId, q.callId, { text: "北京" }), "conflict", "已回答的提问再以文字回答返回 conflict");

      const r2 = await web.submit(sid, "ask 还要订酒店吗？| 要 | 不要");
      const [own] = await v.waitFor("own question", () => (v.pendingQuestions().length ? v.pendingQuestions() : null));
      c.check(v.typedAnswerTarget(childView) === undefined, "父 Session 自己有提问时不选中");
      const typed = await web.submit(sid, "不要");
      c.check(typed.answered === own.callId, "文字提交给父 Session，作为它自己的提问的回答");
      await c.runEnd(v, r2.runId);
      return { sessionId: sid };
    },
  },
  {
    id: "faults",
    title: "服务端故障注入",
    description:
      "需要 serve -faults（ADR-0033）。模型调用失败后 Worker 重试、Run 照常完成；Worker 崩溃后租约过期被接管（新的 Attempt），Run 完成。",
    async run(c) {
      const probe = await c.http("GET", "/v1/debug/faults");
      if (probe.status !== 200) c.skip(`服务端故障注入未开启（GET /v1/debug/faults → ${probe.status}）`);
      const web = await c.device("web", { preset: "web" });
      const sid = await c.newSession(web);
      const v = c.view(web, sid);
      let rule = await c.http("POST", "/v1/debug/faults", { body: { kind: "model_error", session_id: sid, count: 1 } });
      c.check(rule.status === 201, "注入：下一次模型调用返回 503");
      let r = await web.submit(sid, "第一句");
      let end = await c.runEnd(v, r.runId, 30_000);
      const fired = await c.http("GET", "/v1/debug/faults");
      c.check(
        end.kind === "run_completed" && fired.json.recent.some((f) => f.rule_id === rule.json.id && f.session_id === sid),
        "故障已注入，Run 仍然完成（Worker 重试了这一步）",
      );
      rule = await c.http("POST", "/v1/debug/faults", { body: { kind: "worker_crash", session_id: sid, count: 1 } });
      c.check(rule.status === 201, "注入：Worker 写完一步后崩溃（不释放租约）");
      r = await web.submit(sid, "第二句");
      end = await c.runEnd(v, r.runId, 45_000);
      const attempts = v
        .payloads("attempt_started")
        .filter((a) => a.run_id === r.runId)
        .map((a) => a.attempt);
      c.check(end.kind === "run_completed" && attempts.length >= 2, `租约过期后被接管，Run 完成（Attempt ${attempts.join("、")}）`);
      return { sessionId: sid };
    },
  },
  {
    id: "media",
    title: "图片与设备返回的工件",
    description: "用户上传的图片作为工件随输入提交（Media 只引用 artifact://）；手机拍照（需审批）把照片上传为工件，作为调用结果回到 Run。",
    async run(c) {
      const phone = await c.device("phone", { preset: "phone" });
      const web = await c.device("web", { preset: "web" });
      const sid = await c.newSession(web);
      const wv = c.view(web, sid);
      const png = Uint8Array.from(
        atob("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="),
        (x) => x.charCodeAt(0),
      );
      const block = await web.uploadArtifact(sid, "desk.png", "image/png", png);
      c.check(block.media.uri.startsWith("artifact://"), `上传为工件 ${block.media.uri}`);
      const r = await web.submitInput(sid, [{ text: { text: "看看这张图" } }, block]);
      await c.runEnd(wv, r.runId);
      const media = wv
        .payloads("run_requested")
        .find((p) => p.run_id === r.runId)
        .input.find((b) => b.media)?.media;
      c.check(media?.uri === block.media.uri && media.mime_type === "image/png", "输入中的图片只引用工件");
      const t = await c.submitTool(web, wv, `call ${phone.label}__take_photo`);
      const ap = await wv.waitFor("approval", () => wv.pendingApprovals().find((a) => a.runId === t.runId));
      await web.decide(sid, ap.callId, true);
      c.step("网页上批准拍照");
      await c.runEnd(wv, t.runId, 20_000);
      const res = wv.payloads("tool_result").find((p) => p.run_id === t.runId);
      c.check(
        res?.content?.some((b) => b.media?.uri?.startsWith("artifact://")),
        "拍照的结果带回了照片工件",
      );
      return { sessionId: sid };
    },
  },
];

/** 运行一个场景；返回 {ok, ms, error, sessionId}。 */
export async function runScenario(s, env, log, timeoutMs = 60_000) {
  const c = new Context(env, log);
  const start = Date.now();
  let t;
  try {
    const out = await Promise.race([
      s.run(c),
      new Promise((_, reject) => {
        t = setTimeout(() => reject(new Error(`场景超时（${timeoutMs / 1000} 秒）`)), timeoutMs);
      }),
    ]);
    return { ok: true, ms: Date.now() - start, sessionId: out?.sessionId ?? c.sessions[0] ?? "" };
  } catch (e) {
    if (e instanceof SkipError) {
      log(`跳过：${e.message}`);
      return { ok: true, skipped: true, ms: Date.now() - start, sessionId: c.sessions[0] ?? "" };
    }
    log(`✗ ${e instanceof Error ? e.message : String(e)}`);
    return { ok: false, ms: Date.now() - start, error: String(e?.message ?? e), sessionId: c.sessions[0] ?? "" };
  } finally {
    clearTimeout(t);
    c.cleanup();
  }
}
