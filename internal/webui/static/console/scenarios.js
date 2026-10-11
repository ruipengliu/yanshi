// 验证场景：用模拟设备（device.js）对真实网关逐项验证多端语义。每个场景新建自己的设备与 Session，结束时断开设备；
// Session 保留，可在控制台中打开查看事件。Agent 须使用脚本化模型（agents/console.yaml，internal/model/script）。
//
// 浏览器中由控制台的"验证"页运行；make check 中由 internal/e2e 的 TestConsoleScenarios 以 Node.js 运行
// （internal/webui/testdata/run-scenarios.mjs）。不依赖 DOM。

import { DEFAULT_FILES, DEVICE_PRESETS, RequestError, SessionView, SimDevice, newInputId, textOf } from "./device.js";

/**
 * env：
 *   wsUrl        Connection 地址，如 ws://127.0.0.1:8080/v1/connect
 *   httpBase     HTTP API 地址，如 http://127.0.0.1:8080
 *   businessLine 业务线（-auth none 时自报；jwt 时以令牌为准，可为空）
 *   agent        使用脚本化模型的 Agent，默认 console
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
    log(`✗ ${e instanceof Error ? e.message : String(e)}`);
    return { ok: false, ms: Date.now() - start, error: String(e?.message ?? e), sessionId: c.sessions[0] ?? "" };
  } finally {
    clearTimeout(t);
    c.cleanup();
  }
}

