// 验证场景：用模拟设备（device.js）对真实网关逐项验证多端语义。每个场景新建自己的设备与 Session，结束时断开设备；
// Session 保留，可在控制台中打开查看事件。Agent 须使用脚本化模型（agents/console.yaml，internal/model/script）。
//
// 浏览器中由控制台的"验证"页运行；make check 中由 internal/e2e 的 TestConsoleScenarios 以 Node.js 运行
// （internal/webui/testdata/run-scenarios.mjs）。不依赖 DOM。

import {
  DEFAULT_FILES,
  DEVICE_PRESETS,
  RequestError,
  SessionView,
  SimDevice,
  newInputId,
  routeInput,
  textOf,
  waitUntil,
} from "./device.js";

/**
 * env：
 *   wsUrl        Connection 地址，如 ws://127.0.0.1:8080/v1/connect
 *   httpBase     HTTP API 地址，如 http://127.0.0.1:8080
 *   businessLine 业务线（-auth none 时自报；jwt 时以令牌为准，可为空）
 *   agent        使用脚本化模型的 Agent，默认 console
 *   helper       委托场景的目标 Agent（脚本化模型），默认 console-helper
 *   capture      开启旁听的 Agent（脚本化模型，白名单含 captures），默认 console-capture
 *   users        [{endUser, token}, {endUser, token}]：EndUser A 与 B（B 只用于隔离检查）
 */
export class Context {
  constructor(env, log) {
    this.env = { agent: "console", capture: "console-capture", ...env };
    this.log = log;
    this.devices = [];
    this.views = [];
    this.sessions = [];
    // 场景超时后 run 仍在后台继续：之后的等待、创建设备、Session 与故障规则一律以错误结束，不再留下资源。
    this.abort = new AbortController();
    this.signal = this.abort.signal;
  }

  /** 场景已超时：中止之后的步骤。 */
  cancel() {
    this.abort.abort();
  }

  alive() {
    if (this.signal.aborted) throw new Error("场景已超时，停止后续步骤");
  }

  step(msg) {
    this.alive();
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

  async sleep(ms) {
    this.alive();
    await new Promise((r) => setTimeout(r, ms));
    this.alive();
  }

  /** 等待 pred() 为真：views 中任一视图变化时检查。场景超时后以错误结束。 */
  until(what, pred, views, timeoutMs = 10_000) {
    return waitUntil(
      what,
      pred,
      timeoutMs,
      (fn) => {
        const offs = views.map((v) => v.on(fn));
        return () => offs.forEach((off) => off());
      },
      this.signal,
    );
  }

  /** 新建并连接一台设备。name 在同一 EndUser 下决定设备 ID（重复运行时复用同一 Node，标签不变）。 */
  async device(name, { preset = "web", user = 0, capabilities, ...rest } = {}) {
    this.alive();
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
    // 超时之后才连上的设备由 cleanup 之后的这一步断开（cleanup 已经运行过）。
    if (this.signal.aborted) d.disconnect();
    this.step(`设备 ${d.label}（${d.cfg.kind}${d.cfg.clientOnly ? "，只作会话客户端" : ""}）已连接`);
    return d;
  }

  /** 订阅 Session，返回该设备上的视图。 */
  view(device, sid, after = 0n) {
    this.alive();
    const v = new SessionView(device, sid);
    v.signal = this.signal;
    this.views.push(v);
    device.subscribe(sid, after);
    return v;
  }

  /**
   * 子 Session 的视图（委托），按需订阅。Worker 先提交 ToolCallStarted、再创建子 Session：订阅可能以 not_found 结束，
   * 稍后从已收到的位置重订，同对话面板（app.js 的 viewOf）。
   */
  children(device) {
    const views = new Map();
    return (sid) => {
      let v = views.get(sid);
      if (v) return v;
      v = this.view(device, sid);
      views.set(sid, v);
      v.on((kind) => {
        if (kind !== "ended" || (v.ended !== "not_found" && v.ended !== "error")) return;
        setTimeout(() => {
          if (this.signal.aborted || device.subs.has(sid)) return;
          v.ended = "";
          device.subscribe(sid, v.lastSeq());
        }, 200);
      });
      return v;
    };
  }

  async newSession(device, agent = this.env.agent) {
    this.alive();
    const sid = await device.createSession(agent);
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
      this.alive();
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
    this.alive();
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

/**
 * typed 模拟对话面板发送输入框里的文字（chat.js send）：去向由 routeInput 按 typedAnswerTarget 决定，子 Agent 的提问在等
 * 回答时以 answer 发往子 Session，否则提交给本 Session。返回 {route, result}，请求失败时抛出。
 */
async function typed(device, view, childView, text) {
  const route = routeInput({ text }, view.typedAnswerTarget(childView));
  if (route.kind === "answer") return { route, result: await device.answer(route.question.sessionId, route.question.callId, { text }) };
  return { route, result: await device.submit(view.sessionId, text) };
}

/** 父 Session 中的输入（RunRequested 与 Steered）数：文字改投父 Session 时会多一条。 */
function inputs(view) {
  return view.payloads("run_requested").length + view.payloads("steered").length;
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
      // snapshot 加上其后的增量，应恰好是脚本输出的全部 chunk：不漏、不重、顺序正确。
      const want = Array.from({ length: 150 }, (_, i) => `chunk${i} `).join("");
      const joined = (v) => {
        let text = "";
        for (const d of v.deltas.filter((x) => x.run_id === r.runId)) {
          if (d.end) break;
          text = d.snapshot ? (d.text ?? "") : text + (d.text ?? "");
        }
        return text;
      };
      const got = joined(wv);
      c.check(got === want, `网页拼出的草稿与脚本输出的 150 段一致（${got.length}/${want.length} 字符）`);
      c.check(joined(pv) === want, "从头订阅的手机拼出的草稿同样一致");
      c.check(contiguous(wv) && Number(wv.events[0].seq) === 1, "网页补齐了订阅之前的全部事件");
      c.check(wv.reply(r.runId) === "streamed", "网页看到提交的回复");
      return { sessionId: sid };
    },
  },
  {
    id: "resume",
    title: "断线续传",
    description:
      "生成中途网络中断：设备自动重连，按最后收到的 seq 续订，事件不漏、不重。断线期间 Run 被中断时，重连后草稿随 Run 的终态清除。",
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

      // 断线期间 Run 被中断：手机收不到 end，重连后补发的只有终态事件，草稿随 Run 结束清除。
      const r2 = await phone.submit(sid, "stream 400");
      await pv.waitFor("second draft", () => pv.draft.runId === r2.runId && pv.draft.text.length > 0);
      phone.disconnect();
      c.step("手机下线，网页中断 Run");
      await web.interrupt(sid, r2.runId);
      await c.runEnd(wv, r2.runId);
      phone.connect();
      await c.runEnd(pv, r2.runId);
      c.check(pv.draft.runId === "" && pv.draft.text === "", "重连后补发了 Run 的终态，草稿随之清除");
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
    title: "设备离线：宽限期内上线",
    description:
      "调用离线的电脑：Run 挂起、不占用 Worker，投影显示在等离线的设备；电脑在宽限期内上线（ADR-0035），收到调用，Run 继续完成，" +
      "与没有后台调用时相同。",
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
      c.step("电脑在宽限期内上线");
      await c.runEnd(pv, r.runId);
      c.check(pv.reply(r.runId) === `done: ${DEFAULT_FILES["todo.txt"]}`, "电脑上线后执行，Run 完成");
      c.check(pv.backgroundCalls().length === 0 && !pv.payloads("tool_result").some((p) => p.backgrounded), "调用没有转入后台");
      return { sessionId: sid };
    },
  },
  {
    id: "background",
    title: "设备离线超过宽限期：转入后台",
    description:
      "电脑离线超过宽限期：调用转入后台（ADR-0035），模型得到\"已排队\"的结果，Run 完成，对话不再被阻塞；期间的输入照常得到回应。" +
      "电脑上线后执行排队的调用，结果作为平台的消息开启新的一轮，由模型报告。",
    async run(c) {
      const mac = await c.device("mac", { preset: "desktop" });
      const phone = await c.device("phone", { preset: "phone" });
      mac.disconnect();
      c.step("电脑下线");
      const sid = await c.newSession(phone);
      const pv = c.view(phone, sid);
      const r = await c.submitTool(phone, pv, `call ${mac.label}__read_file {"path":"todo.txt"}`);
      const end = await c.runEnd(pv, r.runId, 20_000);
      const bg = pv.payloads("tool_result").find((p) => p.run_id === r.runId && p.backgrounded);
      c.check(end.kind === "run_completed" && bg?.backgrounded?.node_id === mac.id, "宽限期过后调用转入后台，Run 照常完成");
      c.check(pv.reply(r.runId).includes("queued"), `模型得到"已排队"的结果（${pv.reply(r.runId).slice(0, 40)}…）`);
      const [open] = pv.backgroundCalls();
      c.check(open?.callId === bg.call_id && open.capability === `${mac.label}__read_file`, "会话里列出这个未了结的后台调用");
      const st = await c.http("GET", `/v1/sessions/${sid}`);
      const b = st.json?.background?.[0];
      c.check(b?.call_id === bg.call_id && b.node_online === false, "投影中的后台调用：电脑离线");

      const r2 = await phone.submit(sid, "还在吗");
      await c.runEnd(pv, r2.runId);
      c.check(!r2.steered && pv.reply(r2.runId) === "echo: 还在吗", "期间的输入开启新的一轮并得到回应");

      mac.connect();
      c.step("电脑上线");
      const late = await pv.waitFor(
        "background result",
        () => pv.payloads("run_requested").find((p) => p.input?.[0]?.background_result?.call_id === bg.call_id),
        20_000,
      );
      await c.runEnd(pv, late.run_id);
      c.check(mac.ledger.get(bg.call_id)?.executions === 1, "电脑执行了排队的调用一次");
      c.check(
        pv.reply(late.run_id).includes("[后台调用结果]") && pv.reply(late.run_id).includes(DEFAULT_FILES["todo.txt"]),
        "设备的结果开启新的一轮，模型（脚本）看到带说明的结果",
      );
      c.check(pv.backgroundCalls().length === 0, "后台调用随之了结");
      const after = await c.http("GET", `/v1/sessions/${sid}`);
      c.check(after.json?.runs?.find((x) => x.run_id === late.run_id)?.from_background === bg.call_id, "这一轮注明由后台调用开启");
      return { sessionId: sid };
    },
  },
  {
    id: "background-cancel",
    title: "取消后台调用",
    description: "转入后台的调用可以单独取消（ADR-0035）：不开启新的一轮，投递撤回，电脑上线后不执行；再次取消返回 conflict。",
    async run(c) {
      const mac = await c.device("mac", { preset: "desktop" });
      const web = await c.device("web", { preset: "web" });
      mac.disconnect();
      c.step("电脑下线");
      const sid = await c.newSession(web);
      const wv = c.view(web, sid);
      const r = await c.submitTool(web, wv, `call ${mac.label}__read_file {"path":"notes.txt"}`);
      await c.runEnd(wv, r.runId, 20_000);
      const [b] = await wv.waitFor("background call", () => (wv.backgroundCalls().length ? wv.backgroundCalls() : null));
      const runs = wv.payloads("run_requested").length;
      await web.cancelBackground(sid, b.callId);
      c.step("网页取消后台调用");
      await wv.waitFor("cancelled", () => wv.payloads("background_cancelled").some((p) => p.call_id === b.callId));
      c.check(wv.backgroundCalls().length === 0 && wv.payloads("run_requested").length === runs, "取消写入日志，不开启新的一轮");
      await rejects(c, () => web.cancelBackground(sid, b.callId), "conflict", "再次取消返回 conflict");
      mac.connect();
      c.step("电脑上线");
      await c.sleep(500);
      c.check(!mac.ledger.has(b.callId), "电脑没有收到已取消的调用");
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
    id: "card-answer",
    title: "等回答时点卡片",
    description:
      "Run 等 ask_user 的回答时在卡片上点按钮：动作作为提问的回答，不写成输入，而是记在回答的 ToolResult.card_action 中；其他设备据此把按钮显示为已选，再点得到 duplicate。",
    async run(c) {
      const phone = await c.device("phone", { preset: "phone" });
      const web = await c.device("web", { preset: "web" });
      const sid = await c.newSession(web);
      const pv = c.view(phone, sid);
      const wv = c.view(web, sid);
      const card = {
        card_id: "alarm",
        title: "闹钟 07:00",
        image: "icon:alarm",
        buttons: [
          { id: "ok", label: "好的" },
          { id: "later", label: "改到 7:30" },
        ],
      };
      const r = await web.submit(sid, `call show_card ${JSON.stringify(card)}\nask 还要设置别的吗？| 不用了 | 再加一个`);
      const [q] = await pv.waitFor("question on the phone", () => (pv.pendingQuestions().length ? pv.pendingQuestions() : null));
      const shown = pv.cards().get("alarm");
      c.check(shown && !pv.cardActions().has(`${shown.card.call_id}/ok`), "先展示卡片、再提问；按钮还没有选过");
      const action = { card_id: "alarm", call_id: shown.card.call_id, action: "ok", seq: shown.seq };
      const a = await phone.submitCardAction(sid, action);
      c.check(!a.duplicate && a.answered === q.callId && a.runId === r.runId, '提问等回答时在手机上点"好的"：动作作为提问的回答');
      await c.runEnd(wv, r.runId);
      const res = wv.payloads("tool_result").find((p) => p.call_id === q.callId);
      c.check(
        res?.card_action?.call_id === shown.card.call_id && res.card_action.action === "ok" && res.card_action.label === "好的",
        "回答的 ToolResult 带规范化后的卡片动作（card_action，标签由平台填写）",
      );
      const inputs = [...wv.payloads("run_requested"), ...wv.payloads("steered")];
      c.check(!inputs.some((p) => p.input?.some((b) => b.card_action)), "这个动作不在任何输入（RunRequested、Steered）中");
      c.check(wv.cardActions().has(`${shown.card.call_id}/ok`), '网页（另一台设备）把"好的"显示为已选');
      c.check(!wv.cardActions().has(`${shown.card.call_id}/later`), "没点过的按钮不显示为已选");
      c.check(wv.reply(r.runId).includes("选择了「好的」"), "模型（脚本）看到的回答是卡片动作的文字");
      const again = await web.submitCardAction(sid, action);
      c.check(again.duplicate && again.answered === q.callId, "网页上再点一次：duplicate，回答的仍是那个提问");
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
      const childView = c.children(web);
      const helper = c.env.helper ?? "console-helper";
      const r = await web.submit(sid, `call agent__${helper} {"task":"ask 去哪个城市？| 北京 | 上海"}`);
      // 和对话面板一样：订阅委托出去的子 Session，它的变化才会让父 Session 的待回答列表更新。
      const d = await v.waitFor("delegation", () => v.delegations()[0]);
      const cv = childView(d.childSessionId);
      const [q] = await c.until(
        "child question",
        () => {
          const { questions } = v.pendingWithDelegations(childView);
          return questions.length ? questions : null;
        },
        [v, cv],
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
      "子 Agent 的提问是唯一待回答的提问时，输入框的文字（去向由对话面板与场景共用的 routeInput 决定）作为它的回答发往子 Session，" +
      "父 Run 随委托完成，父 Session 没有新增输入；另一台设备抢先回答时为 conflict，文字不改投父 Session。" +
      "带附件、界面上下文或附件上传中时不作为回答；父 Session 自己有提问时，文字照旧提交给父 Session。",
    async run(c) {
      const web = await c.device("web", { preset: "web" });
      const other = await c.device("web2", { preset: "web" });
      const sid = await c.newSession(web);
      const v = c.view(web, sid);
      const childView = c.children(web);
      const helper = c.env.helper ?? "console-helper";
      const ask = `call agent__${helper} {"task":"ask 去哪个城市？| 北京 | 上海"}`;
      const r = await web.submit(sid, ask);
      const d = await v.waitFor("delegation", () => v.delegations()[0]);
      const cv = childView(d.childSessionId);
      const q = await c.until("typed answer target", () => v.typedAnswerTarget(childView), [v, cv]);
      c.check(
        q.question === "去哪个城市？" && q.sessionId === d.childSessionId && q.via === q.sessionId,
        "输入框的文字选中子 Agent 的提问",
      );
      c.check(
        routeInput({ text: "上海", attachments: 1 }, q).kind === "submit" &&
          routeInput({ text: "上海", uiContext: true }, q).kind === "submit",
        "带附件或界面上下文时不作为回答，照旧提交给父 Session",
      );
      c.check(routeInput({ text: "上海", uploading: 1 }, q).kind === "wait", "附件还在上传时先等它们完成，再决定去向");
      // 规则本身：两个待回答的提问时不选中。网关上同一时刻只有一条委托链在推进（扇出至多为 1，调用按顺序进行），
      // 脚本化模型造不出两个，这里在子 Session 的视图上多放一个提问来检查规则。
      const twice = (s) => {
        const real = childView(s);
        return Object.assign(Object.create(real), { pendingQuestions: () => [...real.pendingQuestions(), { ...q, callId: "second" }] });
      };
      c.check(v.typedAnswerTarget(twice) === undefined, "两个待回答的提问时不选中，文字照旧提交给父 Session");
      const before = inputs(v);
      const sent = await typed(web, v, childView, "上海");
      c.check(sent.route.kind === "answer" && sent.route.question.callId === q.callId, "文字以 answer 发往子 Session：上海");
      const end = await c.runEnd(v, r.runId, 30_000);
      c.check(end.kind === "run_completed" && v.reply(r.runId).includes("上海"), "回答经子 Run 写回，父 Run 完成");
      c.check(inputs(v) === before, "父 Session 没有新增输入");
      c.check(v.typedAnswerTarget(childView) === undefined, "提问收起后不再选中");
      await rejects(c, () => web.answer(q.sessionId, q.callId, { text: "北京" }), "conflict", "已回答的提问再以文字回答返回 conflict");

      // 去向已经选定（子 Agent 的提问），另一台设备抢先回答：文字的回答得到 conflict，不改投父 Session。
      const r2 = await web.submit(sid, ask);
      const d2 = await v.waitFor("second delegation", () => v.delegations().find((x) => x.runId === r2.runId));
      const cv2 = childView(d2.childSessionId);
      const q2 = await c.until("second typed answer target", () => v.typedAnswerTarget(childView), [v, cv2]);
      const route = routeInput({ text: "上海" }, q2);
      c.check(route.kind === "answer" && route.question.sessionId === d2.childSessionId, "第二次委托：文字的去向是子 Agent 的提问");
      await other.answer(q2.sessionId, q2.callId, { selected: ["1"] });
      c.step("另一台设备抢先回答：北京");
      const before2 = inputs(v);
      await rejects(
        c,
        () => web.answer(route.question.sessionId, route.question.callId, { text: "上海" }),
        "conflict",
        "文字的回答返回 conflict（对话面板保留文字并提示）",
      );
      const end2 = await c.runEnd(v, r2.runId, 30_000);
      c.check(end2.kind === "run_completed" && v.reply(r2.runId).includes("北京"), "父 Run 以另一台设备的回答完成");
      c.check(inputs(v) === before2, "父 Session 没有新增输入（没有 RunRequested 或 Steered）");

      const r3 = await web.submit(sid, "ask 还要订酒店吗？| 要 | 不要");
      const [own] = await v.waitFor("own question", () => (v.pendingQuestions().length ? v.pendingQuestions() : null));
      c.check(v.typedAnswerTarget(childView) === undefined, "父 Session 自己有提问时不选中");
      const t = await typed(web, v, childView, "不要");
      c.check(t.route.kind === "submit" && t.result.answered === own.callId, "文字提交给父 Session，作为它自己的提问的回答");
      await c.runEnd(v, r3.runId);
      return { sessionId: sid };
    },
  },
  {
    id: "delegation-together",
    title: "同一轮的委托与提问",
    description:
      "模型同一轮返回委托与 ask_user（脚本命令 together）：Worker 按顺序推进，父 Session 的提问等委托结束才派发。" +
      "在那之前它不算待回答，输入框的文字选中子 Agent 的提问；委托结束后父 Session 的提问才待回答，文字作为它的回答。",
    async run(c) {
      const web = await c.device("web", { preset: "web" });
      const sid = await c.newSession(web);
      const v = c.view(web, sid);
      const childView = c.children(web);
      const helper = c.env.helper ?? "console-helper";
      const r = await web.submit(
        sid,
        `together\ncall agent__${helper} {"task":"ask 去哪个城市？| 北京 | 上海"}\nask 还要订酒店吗？| 要 | 不要`,
      );
      const d = await v.waitFor("delegation", () => v.delegations()[0]);
      const cv = childView(d.childSessionId);
      const q = await c.until("typed answer target", () => v.typedAnswerTarget(childView), [v, cv]);
      const m = v.payloads("assistant_message").find((p) => p.run_id === r.runId);
      c.check(m?.tool_calls?.length === 2 && m.tool_calls[1].capability === "ask_user", "模型同一轮返回委托与提问");
      c.check(v.pendingQuestions().length === 0, "委托结束之前父 Session 的提问尚未派发，不算待回答");
      c.check(q.sessionId === d.childSessionId && q.question === "去哪个城市？", "输入框的文字选中子 Agent 的提问");
      const sent = await typed(web, v, childView, "上海");
      c.check(sent.route.kind === "answer", "文字以 answer 发往子 Session");
      const [own] = await v.waitFor("parent question", () => (v.pendingQuestions().length ? v.pendingQuestions() : null), 20_000);
      c.check(
        own.question === "还要订酒店吗？" && v.payloads("tool_result").some((p) => p.call_id === d.callId),
        "委托结束后父 Session 的提问才派发",
      );
      c.check(v.typedAnswerTarget(childView) === undefined, "此时文字不再发往子 Session");
      const t = await typed(web, v, childView, "不要");
      c.check(t.route.kind === "submit" && t.result.answered === own.callId, "文字作为父 Session 自己的提问的回答");
      const end = await c.runEnd(v, r.runId, 30_000);
      c.check(end.kind === "run_completed", "父 Run 完成");
      return { sessionId: sid };
    },
  },
  {
    id: "delegation-approval",
    title: "子 Agent 审批",
    description:
      "子 Run 调用高风险能力（电脑的 write_file）：审批写在子 Session 里，父 Session 的界面一并展示（pendingWithDelegations，" +
      "sessionId 为子 Session）；批准发往子 Session 后电脑执行，父 Run 完成。",
    async run(c) {
      const mac = await c.device("mac", { preset: "desktop" });
      const web = await c.device("web", { preset: "web" });
      const sid = await c.newSession(web);
      const v = c.view(web, sid);
      const childView = c.children(web);
      const helper = c.env.helper ?? "console-helper";
      const task = `call ${mac.label}__write_file ${JSON.stringify({ path: "child.txt", content: "子 Agent 写的" })}`;
      let r;
      let a;
      // 新登记的设备至多 10 秒后才出现在工具中（Catalog.NodeCacheTTL）：子 Run 回复 no such tool 时稍后重试。
      for (let i = 0; ; i++) {
        r = await web.submit(sid, `call agent__${helper} ${JSON.stringify({ task })}`);
        const d = await v.waitFor("delegation", () => v.delegations().find((x) => x.runId === r.runId));
        const cv = childView(d.childSessionId);
        a = await c.until(
          "child approval",
          () => v.pendingWithDelegations(childView).approvals[0] ?? (v.runEnd(r.runId) ? "ended" : undefined),
          [v, cv],
          20_000,
        );
        if (a !== "ended") break;
        if (!v.reply(r.runId).includes("no such tool") || i >= 6) c.check(false, `子 Agent 没有请求审批（${v.reply(r.runId)}）`);
        c.step("工具尚未出现在子 Agent 的目录缓存中，2 秒后重试");
        await c.sleep(2000);
      }
      c.check(
        a.sessionId !== sid && a.agent === helper && a.via === a.sessionId && a.capability === `${mac.label}__write_file`,
        "父 Session 的界面看到子 Agent 的审批，发往子 Session",
      );
      c.check(v.pendingApprovals().length === 0, "审批写在子 Session 里，父 Session 自身没有待审批");
      c.check(!mac.ledger.has(a.callId), "批准之前电脑没有收到调用");
      await web.decide(a.sessionId, a.callId, true);
      c.step("向子 Session 批准");
      const end = await c.runEnd(v, r.runId, 30_000);
      c.check(end.kind === "run_completed" && mac.files["child.txt"] === "子 Agent 写的", "电脑执行了子 Agent 的调用，父 Run 完成");
      c.check(v.pendingWithDelegations(childView).approvals.length === 0, "审批随之收起");
      await rejects(c, () => web.decide(a.sessionId, a.callId, true), "conflict", "再次审批同一调用返回 conflict");
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
  {
    id: "captures",
    title: "旁听",
    description:
      "手机以 HostApp 的身份提交一批旁听转写：网页实时看到带说话人的转写，不开启 Run；同一 capture_id 再交为 duplicate；" +
      "未开启旁听的 Agent 拒绝（invalid）；Run 经 captures 工具读出，佩戴者与他人的话分别注明。",
    async run(c) {
      const phone = await c.device("phone", { preset: "phone" });
      const web = await c.device("web", { preset: "web" });
      const sid = await c.newSession(phone, c.env.capture);
      const wv = c.view(web, sid);
      const segs = [
        { speaker: "wearer", text: "明天我把报价发给张三" },
        { speaker: "other", key: "A", text: "好的，周五之前" },
      ];
      const r = await phone.submitCaptures(sid, segs);
      c.check(!r.duplicate && r.redacted === 0, "手机提交了一批两段的旁听");
      const got = await wv.waitFor("capture on the web", () => wv.payloads("capture_appended")[0]);
      c.check(
        got.capture_id === r.captureId &&
          got.segments?.[0]?.speaker === "SPEAKER_WEARER" &&
          got.segments?.[1]?.speaker === "SPEAKER_OTHER" &&
          got.segments?.[1]?.speaker_key === "A",
        "网页看到同一批转写，说话人是结构化字段",
      );
      c.check(wv.payloads("run_requested").length === 0, "旁听不开启 Run");
      const again = await phone.submitCaptures(sid, segs, { captureId: r.captureId });
      c.check(again.duplicate, "同一 capture_id 再交返回 duplicate");
      const plain = await c.newSession(phone);
      await rejects(c, () => phone.submitCaptures(plain, segs), "invalid", "未开启旁听的 Agent 拒绝旁听");
      const t = await web.submit(sid, "call captures {}");
      await c.runEnd(wv, t.runId);
      const reply = wv.reply(t.runId);
      c.check(
        reply.includes("[佩戴者] 明天我把报价发给张三") && reply.includes("[他人 A] 好的，周五之前"),
        "Run 经 captures 读出转写，注明说话人",
      );
      c.check(wv.payloads("capture_appended").length === 1, "日志中只有一批旁听");
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
        t = setTimeout(() => {
          // run 不会因为 race 输了而停下：置位取消，它之后的步骤一律失败，不再创建设备、Session 与故障规则。
          c.cancel();
          reject(new Error(`场景超时（${timeoutMs / 1000} 秒）`));
        }, timeoutMs);
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
