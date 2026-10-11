// 调试控制台（docs/design/console.md）：在一个页面里模拟多台设备接入网关，观察并操作同一 Session，
// 检查每一帧、每个事件与投影，并运行验证场景。设备与协议见 device.js，场景见 scenarios.js。
import { DEVICE_PRESETS, MODES, SessionView, SimDevice, newInputId, payloadOf, randomHex, textOf } from "/console/device.js";
import { SCENARIOS, runScenario } from "/console/scenarios.js";

const $ = (id) => document.getElementById(id);
const el = (tag, attrs = {}, ...children) => {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") e.className = v;
    else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else if (v !== undefined && v !== null && v !== false) e.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children) if (c !== null && c !== undefined) e.append(c);
  return e;
};

const WS_URL = `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/v1/connect`;
const HTTP = location.origin;
$("gw").textContent = WS_URL;

// —— 持久化的设置：刷新后恢复设备（设备 ID 不变，在网关看来是同一台设备重新上线） ——

const SAVED = "yanshi.console";
const saved = JSON.parse(localStorage.getItem(SAVED) ?? "null") ?? {};
const S = {
  bl: saved.bl ?? "demo",
  agent: saved.agent ?? "console",
  auth: saved.auth ?? "none",
  users: saved.users ?? [
    { name: "u1", token: "" },
    { name: "u2", token: "" },
  ],
  records: saved.records ?? [],
  sid: saved.sid ?? "",
  inspector: saved.inspector ?? "",
};
function persist() {
  localStorage.setItem(
    SAVED,
    JSON.stringify({ bl: S.bl, agent: S.agent, auth: S.auth, users: S.users, records: S.records, sid: S.sid, inspector: S.inspector }),
  );
}

// 链接中的令牌：/console#token.u1=<令牌>&token.u2=<令牌>，用于在手机等不便粘贴的设备上打开（-auth jwt）。
// 放在片段中，不随请求发给服务端；读入后从地址栏清除。
{
  const h = new URLSearchParams(location.hash.slice(1));
  let found = false;
  for (const [k, v] of h) {
    const name = k.startsWith("token.") ? k.slice(6) : "";
    if (!name || !v) continue;
    found = true;
    const u = S.users.find((x) => x.name === name);
    if (u) u.token = v;
    else S.users.push({ name, token: v });
  }
  if (found) {
    S.auth = "jwt";
    persist();
    history.replaceState(null, "", location.pathname);
  }
}

/** id → {rec, dev: SimDevice, view: SessionView | null, ui, lastInput} */
const devices = new Map();
let selected = ""; // 事件视角
const frames = [];
let selectedFrame = null;
let httpEvents = null; // "从 HTTP 拉取"的事件

// —— 顶栏设置 ——

$("bl").value = S.bl;
$("agent").value = S.agent;
$("auth").value = S.auth;
$("bl").onchange = () => {
  S.bl = $("bl").value.trim();
  persist();
  for (const d of devices.values()) applyIdentity(d);
};
$("agent").onchange = () => {
  S.agent = $("agent").value.trim();
  persist();
};
$("auth").onchange = () => {
  S.auth = $("auth").value;
  persist();
  renderUsers();
  for (const d of devices.values()) applyIdentity(d);
};

function token(user) {
  return S.auth === "jwt" ? (S.users.find((u) => u.name === user)?.token ?? "") : "";
}

/** jwt 时身份以令牌为准，Hello 中不再自报。重新连接后生效。 */
function applyIdentity(d) {
  const jwt = S.auth === "jwt";
  Object.assign(d.dev.cfg, {
    businessLine: jwt ? "" : S.bl,
    endUser: jwt ? "" : d.rec.user,
    token: token(d.rec.user),
  });
}

// —— EndUser ——

function renderUsers() {
  $("jwt-hint").hidden = S.auth !== "jwt";
  const box = $("users");
  box.replaceChildren(
    ...S.users.map((u, i) => {
      const name = el("input", { class: "name", value: u.name, title: "EndUser" });
      name.onchange = () => {
        const old = u.name;
        u.name = name.value.trim() || old;
        for (const r of S.records) if (r.user === old) r.user = u.name;
        persist();
        location.reload();
      };
      const tok = el("input", { class: "token", value: u.token, placeholder: "令牌（jwt）", hidden: S.auth !== "jwt" });
      tok.onchange = () => {
        u.token = tok.value.trim();
        persist();
        for (const d of devices.values()) applyIdentity(d);
      };
      const rm = el("button", { class: "tiny", title: "删除用户" }, "✕");
      rm.onclick = () => {
        if (S.records.some((r) => r.user === u.name)) return alert("先移除该用户的设备");
        S.users.splice(i, 1);
        persist();
        renderUsers();
      };
      return el("div", { class: "user-row" }, name, tok, rm);
    }),
  );
  const sel = $("new-user");
  const cur = sel.value;
  sel.replaceChildren(...S.users.map((u) => el("option", { value: u.name }, u.name)));
  if (S.users.some((u) => u.name === cur)) sel.value = cur;
  const st = $("st-user");
  const scur = st.value;
  st.replaceChildren(...S.users.map((u) => el("option", { value: u.name }, u.name)));
  if (S.users.some((u) => u.name === scur)) st.value = scur;
}
$("add-user").onclick = () => {
  let n = S.users.length + 1;
  while (S.users.some((u) => u.name === `u${n}`)) n++;
  S.users.push({ name: `u${n}`, token: "" });
  persist();
  renderUsers();
};

// —— 设备 ——

function newRecord(preset, user) {
  const p = DEVICE_PRESETS[preset];
  return {
    id: `con-${preset}-${randomHex(4)}`,
    preset,
    user,
    label: p.label,
    kind: p.kind,
    clientOnly: !!p.clientOnly,
    caps: p.capabilities.map((name) => ({ name, mode: "auto", delayMs: 300 })),
    wanted: true,
  };
}

function addDevice(rec) {
  const dev = new SimDevice({
    url: WS_URL,
    deviceId: rec.id,
    label: rec.label,
    kind: rec.kind,
    clientOnly: rec.clientOnly,
    capabilities: rec.caps.map((c) => ({ ...c })),
  });
  const d = { rec, dev, view: null, ui: null, lastInput: null };
  applyIdentity(d);
  devices.set(rec.id, d);
  buildDevice(d);
  dev.on((n) => onDeviceNotice(d, n));
  if (rec.wanted) dev.connect();
  if (S.sid) openOn(d);
  renderDeviceList();
  renderViewSelect();
  return d;
}

function removeDevice(d) {
  d.dev.disconnect();
  d.view?.dispose();
  d.ui.root.remove();
  devices.delete(d.rec.id);
  S.records = S.records.filter((r) => r.id !== d.rec.id);
  persist();
  if (selected === d.rec.id) selected = "";
  renderDeviceList();
  renderViewSelect();
}

for (const b of document.querySelectorAll(".add-device")) {
  b.onclick = () => {
    const rec = newRecord(b.dataset.preset, $("new-user").value || S.users[0]?.name || "u1");
    S.records.push(rec);
    persist();
    addDevice(rec);
  };
}

function renderDeviceList() {
  $("device-list").replaceChildren(
    ...[...devices.values()].map((d) => {
      const row = el(
        "div",
        { class: "device-row", title: d.rec.id },
        el("span", { class: `dot ${d.dev.status}` }),
        el("b", {}, d.dev.label || d.rec.label),
        el("span", { class: "muted" }, `${d.rec.kind} · ${d.rec.user}${d.rec.clientOnly ? "" : " · Node"}`),
      );
      row.onclick = () => select(d.rec.id);
      return row;
    }),
  );
  renderFrameDevices();
}

function select(id) {
  selected = id;
  for (const d of devices.values()) d.ui.root.classList.toggle("selected", d.rec.id === id);
  $("ev-view").value = id;
  httpEvents = null;
  scheduleEvents();
  devices.get(id)?.ui.root.scrollIntoView({ behavior: "smooth", inline: "nearest", block: "nearest" });
}

function buildDevice(d) {
  const root = $("device-tpl").content.firstElementChild.cloneNode(true);
  const q = (s) => root.querySelector(s);
  const ui = {
    root,
    dot: q(".dot"),
    label: q(".dev-label"),
    meta: q(".dev-meta"),
    online: q(".dev-online"),
    focus: q(".dev-focus"),
    presence: q(".dev-presence"),
    log: q(".dev-log"),
    pending: q(".dev-pending"),
    input: q("textarea"),
    templates: q(".dev-templates"),
    caps: q(".caps"),
    ledger: q(".ledger"),
    node: q(".dev-node"),
    draft: null,
  };
  d.ui = ui;
  root.addEventListener("mousedown", () => select(d.rec.id));
  ui.online.onclick = () => {
    if (d.dev.wanted) d.dev.disconnect();
    else d.dev.connect();
    d.rec.wanted = d.dev.wanted;
    persist();
  };
  q(".dev-drop").onclick = () => d.dev.drop();
  q(".dev-remove").onclick = () => removeDevice(d);
  ui.focus.onchange = () => S.sid && d.dev.setActivity(S.sid, ui.focus.checked);
  q(".dev-send").onclick = () => send(d);
  q(".dev-interrupt").onclick = async () => {
    const run = d.view?.activeRun();
    if (!run) return note(d, "没有进行中的 Run");
    try {
      await d.dev.interrupt(S.sid, run);
    } catch (e) {
      note(d, `中断失败：${e.message}`, true);
    }
  };
  let typingAt = 0;
  ui.input.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      send(d);
    }
  });
  ui.input.addEventListener("input", () => {
    // "正在输入"每 3 秒重发一次：网关 10 秒后视为停止。
    if (!S.sid || Date.now() - typingAt < 3000) return;
    typingAt = Date.now();
    d.dev.setActivity(S.sid, ui.focus.checked, true);
  });
  ui.templates.addEventListener("focus", () => renderTemplates(d));
  ui.templates.addEventListener("mousedown", () => renderTemplates(d));
  ui.templates.onchange = () => {
    if (ui.templates.value) {
      ui.input.value = ui.templates.value;
      ui.input.focus();
    }
    ui.templates.value = "";
  };
  q(".opt-resend").onclick = () => {
    if (!d.lastInput) return note(d, "还没有提交过输入");
    submit(d, d.lastInput.text, d.lastInput.inputId);
  };
  if (d.rec.clientOnly) ui.node.remove();
  renderTemplates(d);
  renderCaps(d);
  renderHead(d);
  $("devices").append(root);
}

function renderHead(d) {
  const { ui, dev, rec } = d;
  ui.dot.className = `dot ${dev.status}`;
  ui.label.textContent = dev.label || rec.label;
  const role = rec.clientOnly ? "客户端" : "Node";
  const err = dev.status === "offline" && dev.lastError ? ` · ${dev.lastError}` : "";
  ui.meta.textContent = `${rec.kind} · ${rec.user} · ${role}${err}`;
  ui.meta.title = `设备 ID ${rec.id}`;
  ui.online.textContent = dev.wanted ? "下线" : "上线";
}

function renderTemplates(d) {
  const opts = [["", "命令模板…"]];
  const sample = {
    read_file: '{"path":"notes.txt"}',
    write_file: '{"path":"plan.txt","content":"周五发布"}',
    send_sms: '{"to":"10086","text":"晚上见"}',
  };
  opts.push(["你好", "复述：你好"]);
  opts.push(["stream 200", "长输出 stream 200（插话、中断、晚加入）"]);
  opts.push(["ask 周会改到哪天？| 周四 | 周五", "提问 ask_user"]);
  opts.push(["call clock_now", "进程内能力 clock_now"]);
  const nodes = [...devices.values()].filter((x) => !x.rec.clientOnly && x.rec.user === d.rec.user);
  for (const n of nodes) {
    const label = n.dev.label || n.rec.label;
    for (const c of n.dev.capabilities) {
      const cmd = `call ${label}__${c.name}${sample[c.name] ? " " + sample[c.name] : ""}`;
      opts.push([cmd, `${label} · ${c.name}${c.risk === "high" ? "（需审批）" : ""}`]);
    }
  }
  const phone = nodes.find((n) => n.dev.capabilities.some((c) => c.name === "get_location"));
  const pc = nodes.find((n) => n.dev.capabilities.some((c) => c.name === "write_file"));
  if (phone && pc) {
    const cmd = `call ${phone.dev.label || phone.rec.label}__get_location\ncall ${pc.dev.label || pc.rec.label}__write_file {"path":"where.txt","content":"在北京"}`;
    opts.push([cmd, "多步：手机定位 → 电脑写文件"]);
  }
  d.ui.templates.replaceChildren(...opts.map(([v, t]) => el("option", { value: v }, t)));
}

function renderCaps(d) {
  if (d.rec.clientOnly) return;
  const rows = d.dev.capabilities.map((c) => {
    const mode = el("select", {}, ...MODES.map((m) => el("option", { value: m, selected: c.mode === m }, modeName(m))));
    mode.onchange = () => {
      c.mode = mode.value;
      save(d);
    };
    const delay = el("input", { type: "number", min: 0, step: 100, value: c.delayMs ?? 0, title: "执行耗时（毫秒）" });
    delay.onchange = () => {
      c.delayMs = Number(delay.value) || 0;
      save(d);
    };
    return el(
      "tr",
      {},
      el("td", { title: c.description }, el("b", {}, c.name)),
      el("td", { class: c.risk === "high" ? "risk-high" : "" }, c.risk === "high" ? "需审批" : "低风险"),
      el("td", { title: "幂等：执行中崩溃后可重新执行" }, c.idempotent ? "幂等" : ""),
      el("td", {}, mode),
      el("td", {}, delay, " ms"),
    );
  });
  d.ui.caps.replaceChildren(...rows);
}

function modeName(m) {
  return { auto: "自动执行", manual: "手工填写结果", error: "返回错误", hang: "不回应（超时）", crash: "执行中崩溃" }[m];
}

function save(d) {
  d.rec.caps = d.dev.capabilities.map((c) => ({ name: c.name, mode: c.mode, delayMs: c.delayMs }));
  persist();
}

function renderLedger(d) {
  if (d.rec.clientOnly) return;
  const entries = [...d.dev.ledger.values()].reverse().slice(0, 30);
  d.ui.ledger.replaceChildren(
    ...entries.map((e) => {
      const state = {
        executing: "执行中",
        awaiting: "等待手工结果",
        started: "执行中断（崩溃）",
        done: "已完成",
        cancelled: "已取消",
        new: "新",
      }[e.state];
      const box = el(
        "div",
        { class: `entry ${e.state}` },
        el("div", {}, el("b", {}, e.capability), ` ${e.args}`),
        el(
          "div",
          { class: "muted" },
          `${e.callId} · ${state} · 投递 ${e.deliveries} 次 · 执行 ${e.executions} 次${e.state === "done" ? (e.acked ? " · 已确认" : " · 待确认") : ""}${e.cancelled ? " · 收到 Cancel" : ""}`,
        ),
      );
      if (e.state === "done") box.append(el("div", { class: e.isError ? "note error" : "" }, (e.isError ? "错误：" : "结果：") + e.result));
      if (e.state === "awaiting") {
        const input = el("input", { placeholder: "结果文本" });
        const ok = el("button", { class: "tiny primary" }, "返回结果");
        const bad = el("button", { class: "tiny" }, "返回错误");
        ok.onclick = () => d.dev.finish(e.callId, input.value || "ok", false);
        bad.onclick = () => d.dev.finish(e.callId, input.value || "failed", true);
        box.append(el("div", { class: "row" }, input, ok, bad));
      }
      return box;
    }),
  );
  if (entries.some((e) => e.state === "awaiting")) d.ui.node.open = true;
}

// —— 设备上的 Session 视图 ——

function onDeviceNotice(d, n) {
  switch (n.type) {
    case "status":
      renderHead(d);
      renderDeviceList();
      break;
    case "frame":
      addFrame(d, n);
      break;
    case "ledger":
      renderLedger(d);
      break;
  }
}

function openOn(d) {
  d.view?.dispose();
  d.ui.log.replaceChildren();
  d.ui.pending.replaceChildren();
  d.ui.presence.replaceChildren();
  d.ui.draft = null;
  d.view = null;
  if (!S.sid) return;
  const v = new SessionView(d.dev, S.sid);
  d.view = v;
  v.on((kind) => onViewChange(d, kind));
  d.dev.subscribe(S.sid, 0n);
  if (d.ui.focus.checked) d.dev.setActivity(S.sid, true);
}

function openSession(sid) {
  for (const d of devices.values()) if (S.sid && d.dev.subs.has(S.sid)) d.dev.unsubscribe(S.sid);
  S.sid = sid.trim();
  $("sid").value = S.sid;
  persist();
  httpEvents = null;
  for (const d of devices.values()) openOn(d);
  scheduleEvents();
  refreshProjection();
}

function onViewChange(d, kind) {
  const v = d.view;
  if (kind === "event") {
    const e = v.events.at(-1);
    renderEvent(d, e);
    // 提交的助手消息取代草稿（SessionView 已清除）。
    renderDraft(d);
    renderPending(d);
    if (d.rec.id === selected || !selected) scheduleEvents();
    scheduleProjection();
  } else if (kind === "draft") {
    renderDraft(d);
  } else if (kind === "presence") {
    renderPresence(d);
  } else if (kind === "ended") {
    note(d, `订阅结束：${v.ended}${v.ended === "not_found" ? "（Session 不存在或不属于该设备的 EndUser）" : ""}`, true);
  }
}

function runColor(runId) {
  let h = 0;
  for (const ch of runId ?? "") h = (h * 31 + ch.charCodeAt(0)) % 360;
  return `hsl(${h} 70% 55%)`;
}

function append(d, node) {
  const log = d.ui.log;
  const stick = log.scrollHeight - log.scrollTop - log.clientHeight < 40;
  if (d.ui.draft) log.insertBefore(node, d.ui.draft);
  else log.append(node);
  if (stick) log.scrollTop = log.scrollHeight;
}

function note(d, text, error = false, e = null) {
  const n = el("div", { class: `note${error ? " error" : ""}` }, text);
  if (e) n.onclick = () => showEvent(d, e);
  append(d, n);
}

function renderEvent(d, e) {
  const [kind, p] = payloadOf(e);
  const show = () => showEvent(d, e);
  const tag = (s) => el("span", { class: "tag" }, s);
  switch (kind) {
    case "run_requested":
    case "steered": {
      const ui = (p.input ?? []).find((b) => b.ui_context);
      const b = el(
        "div",
        { class: `msg user${kind === "steered" ? " steer" : ""}`, onclick: show },
        tag(`${kind === "steered" ? "插话 · " : ""}#${e.seq} · ${short(p.run_id)}${p.from_call ? " · 来自通话" : ""}`),
        textOf(p.input),
      );
      if (ui) b.append(tag(`界面上下文：${ui.ui_context.screen ?? ""}`));
      append(d, b);
      break;
    }
    case "assistant_message": {
      const text = textOf(p.content);
      if (text) append(d, el("div", { class: "msg assistant", onclick: show }, tag(`#${e.seq} · ${p.model ?? ""}`), text));
      for (const c of p.tool_calls ?? []) {
        append(d, el("div", { class: "tool", onclick: show }, `→ ${c.capability}(${c.arguments_json ?? ""})\n  ${c.call_id}`));
      }
      break;
    }
    case "tool_call_started":
      note(d, `派发 ${short(p.call_id)} → ${p.node_id}`, false, e);
      break;
    case "tool_result":
      append(
        d,
        el(
          "div",
          { class: `tool ${p.is_error ? "error" : "ok"}`, onclick: show },
          `← ${short(p.call_id)}${p.attempt ? "" : "（外部结果）"}${p.input_id ? "（输入的回答）" : ""}\n${textOf(p.content)}`,
        ),
      );
      break;
    case "approval_requested":
      note(d, `请求审批 ${short(p.call_id)}：${p.summary ?? ""}`, false, e);
      break;
    case "approval_decided":
      note(d, `${p.approved ? "已批准" : "已拒绝"} ${short(p.call_id)}（${p.by ?? ""}）`, false, e);
      break;
    case "attempt_started":
      note(d, `Attempt ${p.attempt} · ${p.worker_id ?? ""}`, false, e);
      break;
    case "run_suspended":
      note(d, `挂起${p.reason ? `（${p.reason}）` : ""}`, false, e);
      break;
    case "run_completed":
      note(d, `✓ Run ${short(p.run_id)} 完成`, false, e);
      break;
    case "run_failed":
      note(d, `✗ Run ${short(p.run_id)} 失败：${p.reason ?? ""}`, true, e);
      break;
    case "run_interrupted":
      note(d, `■ Run ${short(p.run_id)} 被中断（${p.by ?? ""}）`, true, e);
      break;
    default:
      note(d, `${kind} ${summarize(kind, p)}`, kind === "content_moderated", e);
  }
}

function renderDraft(d) {
  const t = d.view.draft.text;
  if (!t) {
    d.ui.draft?.remove();
    d.ui.draft = null;
    return;
  }
  if (!d.ui.draft) {
    d.ui.draft = el("div", { class: "msg assistant partial" });
    d.ui.log.append(d.ui.draft);
  }
  d.ui.draft.textContent = t;
  d.ui.log.scrollTop = d.ui.log.scrollHeight;
}

function renderPresence(d) {
  d.ui.presence.replaceChildren(
    ...d.view.presence
      .filter((v) => v.device_id !== d.rec.id)
      .map((v) =>
        el(
          "span",
          { class: `viewer${v.focused ? " focused" : ""}${v.typing ? " typing" : ""}`, title: v.device_id },
          v.label || v.device_id,
        ),
      ),
  );
  if (d.view.presence.some((v) => v.device_id !== d.rec.id)) d.ui.presence.prepend(el("span", { class: "muted" }, "也在看："));
}

function renderPending(d) {
  const v = d.view;
  const cards = [];
  for (const a of v.pendingApprovals()) {
    const ok = el("button", { class: "tiny primary" }, "批准");
    const no = el("button", { class: "tiny" }, "拒绝");
    const decide = (approve) => d.dev.decide(S.sid, a.callId, approve).catch((e) => note(d, `审批失败：${e.message}`, true));
    ok.onclick = () => decide(true);
    no.onclick = () => decide(false);
    cards.push(
      el(
        "div",
        { class: "card" },
        el("div", {}, "需要审批：", el("b", {}, a.summary || a.capability)),
        el("div", { class: "muted" }, `${a.capability} · ${a.callId}`),
        el("div", { class: "row" }, ok, no),
      ),
    );
  }
  for (const q of v.pendingQuestions()) {
    const card = el("div", { class: "card question" }, el("b", {}, q.question || "（提问）"));
    const chosen = new Set();
    const answer = (a) => d.dev.answer(S.sid, q.callId, a).catch((e) => note(d, `回答失败：${e.message}`, true));
    if (q.options.length) {
      const row = el("div", { class: "row" });
      for (const o of q.options) {
        const b = el("button", { class: "tiny", title: o.description ?? "" }, o.label);
        b.onclick = () => {
          if (!q.multiple) return answer({ selected: [o.id] });
          chosen.has(o.id) ? chosen.delete(o.id) : chosen.add(o.id);
          b.classList.toggle("primary", chosen.has(o.id));
        };
        row.append(b);
      }
      card.append(row);
    }
    const fields = q.fields.map((f) => el("input", { placeholder: `${f.label}${f.required ? "（必填）" : ""}`, "data-name": f.name }));
    for (const f of fields) card.append(f);
    const text = el("input", { placeholder: "或者输入文字回答" });
    const go = el("button", { class: "tiny primary" }, "回答");
    go.onclick = () => {
      const values = {};
      for (const f of fields) if (f.value) values[f.dataset.name] = f.value;
      answer({ selected: [...chosen], values, text: text.value });
    };
    card.append(el("div", { class: "row" }, text, go));
    cards.push(card);
  }
  d.ui.pending.replaceChildren(...cards);
}

async function send(d) {
  const text = d.ui.input.value.trim();
  if (!text) return;
  if (!S.sid) return note(d, "先新建或打开一个 Session", true);
  d.ui.input.value = "";
  d.dev.setActivity(S.sid, d.ui.focus.checked, false);
  await submit(d, text, newInputId());
}

async function submit(d, text, inputId) {
  const root = d.ui.root;
  const opts = { inputId };
  if (root.querySelector(".opt-ui").checked) {
    opts.uiContext = {
      screen: root.querySelector(".opt-ui-screen").value || undefined,
      content: root.querySelector(".opt-ui-content").value || undefined,
    };
  }
  opts.dropAfterSend = root.querySelector(".opt-drop").checked;
  d.lastInput = { text, inputId };
  try {
    const r = await d.dev.submit(S.sid, text, opts);
    const how = r.duplicate
      ? "重复的 input_id：返回首次的结果，没有写入"
      : r.answered
        ? `作为对 ${short(r.answered)} 的回答`
        : r.steered
          ? "并入进行中的 Run（插话）"
          : "开启新 Run";
    note(d, `提交 → ${short(r.runId)}：${how}`);
  } catch (e) {
    note(d, `提交失败：${e.message}`, true);
  }
}

function short(id) {
  if (!id) return "";
  return id.length > 14 ? id.slice(0, 9) + "…" + id.slice(-4) : id;
}

function summarize(kind, p) {
  switch (kind) {
    case "session_created":
      return `${p.business_line ?? ""}/${p.end_user ?? ""} · ${p.agent?.name ?? ""}@${p.agent?.version ?? ""}`;
    case "run_requested":
    case "steered":
      return `${textOf(p.input)}${p.input_id ? `  [${p.input_id}]` : ""}`;
    case "attempt_started":
      return `attempt ${p.attempt} · ${p.worker_id ?? ""}`;
    case "assistant_message": {
      const calls = (p.tool_calls ?? []).map((c) => `→ ${c.capability}(${c.arguments_json ?? ""})`).join(" ");
      return [textOf(p.content), calls].filter(Boolean).join(" ");
    }
    case "tool_call_started":
      return `${p.call_id} → ${p.node_id ?? "进程内"}`;
    case "tool_result":
      return `${p.call_id}${p.is_error ? " [错误]" : ""} ${textOf(p.content)}`;
    case "run_suspended":
      return `${p.reason ?? ""}${p.until ? ` 至 ${p.until}` : ""}`;
    case "approval_requested":
      return `${p.call_id} ${p.summary ?? ""}`;
    case "approval_decided":
      return `${p.call_id} ${p.approved ? "批准" : "拒绝"} · ${p.by ?? ""}`;
    case "run_failed":
      return p.reason ?? "";
    case "run_interrupted":
      return `by ${p.by ?? ""}`;
    case "memory_recalled":
      return `${(p.items ?? []).length} 条`;
    case "content_moderated":
      return `${p.stage ?? ""} ${(p.labels ?? []).join(",")}`;
    case "context_compacted":
      return `至 #${p.through_seq}`;
    case "session_closed":
      return `${p.by ?? ""} ${p.reason ?? ""}`;
    case "call_transcript":
      return `${p.role}: ${p.text ?? ""}`;
    default:
      return "";
  }
}

// —— 检查器：事件 ——

function renderViewSelect() {
  const sel = $("ev-view");
  const cur = selected;
  sel.replaceChildren(
    ...[...devices.values()].map((d) => el("option", { value: d.rec.id }, `${d.dev.label || d.rec.label}（${d.rec.user}）`)),
    el("option", { value: "http" }, "HTTP 拉取"),
  );
  sel.value = httpEvents ? "http" : cur || [...devices.keys()][0] || "http";
}
$("ev-view").onchange = () => {
  if ($("ev-view").value === "http") return fetchHTTPEvents();
  select($("ev-view").value);
};
$("ev-filter").oninput = () => scheduleEvents();
$("ev-http").onclick = () => fetchHTTPEvents();

async function fetchHTTPEvents() {
  if (!S.sid) return;
  const r = await api("GET", `/v1/sessions/${S.sid}/events?after=0&limit=1000`, ownerUser());
  httpEvents = r.json?.events ?? [];
  $("ev-view").value = "http";
  if (r.status !== 200) $("ev-detail").textContent = `HTTP ${r.status}\n${JSON.stringify(r.json, null, 2)}`;
  scheduleEvents();
}

let eventsQueued = false;
function scheduleEvents() {
  if (eventsQueued) return;
  eventsQueued = true;
  requestAnimationFrame(() => {
    eventsQueued = false;
    renderEvents();
  });
}

let selectedEvent = null;
function renderEvents() {
  const d = devices.get(selected) ?? [...devices.values()][0];
  const list = httpEvents ?? d?.view?.events ?? [];
  const f = $("ev-filter").value.trim().toLowerCase();
  const rows = [];
  for (const e of list) {
    const [kind, p] = payloadOf(e);
    const text = summarize(kind, p);
    if (f && !`${kind} ${p.run_id ?? ""} ${p.call_id ?? ""} ${text}`.toLowerCase().includes(f)) continue;
    const time = e.time ? new Date(e.time).toTimeString().slice(0, 8) : "";
    const row = el(
      "div",
      {
        class: `line ev-line run-stripe${selectedEvent === e ? " sel" : ""}`,
        style: `border-left-color:${p.run_id ? runColor(p.run_id) : "transparent"}`,
      },
      el("span", {}, e.seq),
      el("span", { class: "muted" }, time),
      el("span", { class: `k-${kind}` }, kind),
      el("span", { class: "muted", title: p.run_id ?? "" }, short(p.run_id)),
      el("span", { title: text }, text),
    );
    row.onclick = () => {
      selectedEvent = e;
      $("ev-detail").textContent = JSON.stringify(e, null, 2);
      for (const r of $("ev-list").querySelectorAll(".sel")) r.classList.remove("sel");
      row.classList.add("sel");
    };
    rows.push(row);
  }
  const box = $("ev-list");
  const stick = box.scrollHeight - box.scrollTop - box.clientHeight < 40;
  box.replaceChildren(...rows);
  if (stick) box.scrollTop = box.scrollHeight;
  $("ev-count").textContent =
    `${rows.length} / ${list.length} 个事件${httpEvents ? "（HTTP）" : d ? `（${d.dev.label || d.rec.label} 收到的）` : ""}`;
}

function showEvent(d, e) {
  setTab("events");
  if (selected !== d.rec.id) select(d.rec.id);
  selectedEvent = e;
  $("ev-detail").textContent = JSON.stringify(e, null, 2);
  scheduleEvents();
}

// —— 检查器：帧 ——

const MAX_FRAMES = 3000;
function frameKind(m) {
  if (!m) return "（无法解析）";
  const [k] = Object.keys(m);
  const v = m[k] ?? {};
  if (k === "event") return `event.${payloadOf(v)[0]}`;
  if (k === "request") return `request.${Object.keys(v).find((x) => x !== "request_id") ?? ""}`;
  if (k === "response") return v.error ? `response.error` : "response";
  return k;
}

function addFrame(d, n) {
  if ($("fr-pause").checked) return;
  frames.push({
    t: new Date(),
    id: d.rec.id,
    label: d.dev.label || d.rec.label,
    dir: n.dir,
    msg: n.msg,
    raw: n.raw,
    kind: frameKind(n.msg),
  });
  if (frames.length > MAX_FRAMES) frames.splice(0, frames.length - MAX_FRAMES);
  scheduleFrames();
}

let framesQueued = false;
function scheduleFrames() {
  if (framesQueued || $("tab-frames").hidden) return;
  framesQueued = true;
  requestAnimationFrame(() => {
    framesQueued = false;
    renderFrames();
  });
}

function renderFrames() {
  const dev = $("fr-device").value;
  const dir = $("fr-dir").value;
  const f = $("fr-filter").value.trim().toLowerCase();
  const deltas = $("fr-deltas").checked;
  const shown = frames.filter(
    (x) =>
      (!dev || x.id === dev) &&
      (!dir || x.dir === dir) &&
      (deltas || x.kind !== "delta") &&
      (!f ||
        x.kind.toLowerCase().includes(f) ||
        JSON.stringify(x.msg ?? x.raw)
          .toLowerCase()
          .includes(f)),
  );
  const rows = shown.slice(-500).map((x) => {
    const body = JSON.stringify(x.msg ?? x.raw);
    const row = el(
      "div",
      { class: `line fr-line${selectedFrame === x ? " sel" : ""}` },
      el("span", { class: "muted" }, x.t.toTimeString().slice(0, 8)),
      el("span", {}, x.label),
      el("span", { class: `dir-${x.dir}` }, x.dir === "out" ? "↑" : "↓"),
      el("span", {}, x.kind),
      el("span", { class: "muted" }, body.length > 300 ? body.slice(0, 300) + "…" : body),
    );
    row.onclick = () => {
      selectedFrame = x;
      $("fr-detail").textContent = x.msg ? JSON.stringify(x.msg, null, 2) : x.raw;
      for (const r of $("fr-list").querySelectorAll(".sel")) r.classList.remove("sel");
      row.classList.add("sel");
    };
    return row;
  });
  const box = $("fr-list");
  const stick = box.scrollHeight - box.scrollTop - box.clientHeight < 40;
  box.replaceChildren(...rows);
  if (stick) box.scrollTop = box.scrollHeight;
  $("fr-count").textContent = `${shown.length} 帧${shown.length > 500 ? "（显示最近 500）" : ""}`;
}
for (const id of ["fr-device", "fr-dir", "fr-deltas"]) $(id).onchange = () => scheduleFrames();
$("fr-filter").oninput = () => scheduleFrames();
$("fr-clear").onclick = () => {
  frames.length = 0;
  scheduleFrames();
};

function renderFrameDevices() {
  for (const [id, all] of [
    ["fr-device", true],
    ["raw-device", false],
  ]) {
    const sel = $(id);
    const cur = sel.value;
    sel.replaceChildren(
      ...(all ? [el("option", { value: "" }, "全部设备")] : []),
      ...[...devices.values()].map((d) => el("option", { value: d.rec.id }, d.dev.label || d.rec.label)),
    );
    if ([...sel.options].some((o) => o.value === cur)) sel.value = cur;
  }
}

const RAW_TEMPLATES = {
  "": () => "",
  subscribe: () => ({ subscribe: { session_id: S.sid, after_seq: "0" } }),
  "subscribe（从 seq 3 续订）": () => ({ subscribe: { session_id: S.sid, after_seq: "3" } }),
  unsubscribe: () => ({ unsubscribe: { session_id: S.sid } }),
  "activity（前台、正在输入）": () => ({ activity: { session_id: S.sid, focused: true, typing: true } }),
  "request.submit": () => ({
    request: { request_id: "manual-1", submit: { session_id: S.sid, input: [{ text: { text: "你好" } }], input_id: newInputId() } },
  }),
  "request.submit（界面上下文过长，应 invalid）": () => ({
    request: {
      request_id: "manual-2",
      submit: { session_id: S.sid, input: [{ text: { text: "这个" } }, { ui_context: { screen: "x".repeat(200) } }] },
    },
  }),
  "request.create_session": () => ({ request: { request_id: "manual-3", create_session: { agent: S.agent } } }),
  "request.close": () => ({ request: { request_id: "manual-4", close: { session_id: S.sid, reason: "manual" } } }),
  "result（伪造未派发的调用结果）": () => ({ result: { call_id: "call_forged", content: [{ text: { text: "forged" } }] } }),
  "未知字段（应断开连接）": () => ({ bogus: {} }),
};
$("raw-template").replaceChildren(...Object.keys(RAW_TEMPLATES).map((k) => el("option", { value: k }, k || "模板…")));
$("raw-template").onchange = () => {
  const v = RAW_TEMPLATES[$("raw-template").value]();
  $("raw-body").value = v ? JSON.stringify(v, null, 2) : "";
  $("raw-template").value = "";
};
$("raw-send").onclick = () => {
  const d = devices.get($("raw-device").value);
  if (!d) return;
  try {
    d.dev.sendRaw(JSON.parse($("raw-body").value));
  } catch (e) {
    $("fr-detail").textContent = `发送失败：${e.message}`;
  }
};

// —— 检查器：状态 ——

async function api(method, path, user, body) {
  const headers = { "Content-Type": "application/json" };
  const t = token(user);
  if (t) headers.Authorization = `Bearer ${t}`;
  try {
    const res = await fetch(HTTP + path, { method, headers, body: body ? JSON.stringify(body) : undefined });
    const text = await res.text();
    let json = text;
    try {
      json = text ? JSON.parse(text) : null;
    } catch {
      // 非 JSON 响应原样显示。
    }
    return { status: res.status, json };
  } catch (e) {
    return { status: 0, json: String(e) };
  }
}

/** 当前 Session 所属的 EndUser：取已收到其事件的设备的用户（jwt 时用于选择令牌）。 */
function ownerUser() {
  for (const d of devices.values()) if (d.view?.events.length) return d.rec.user;
  return $("st-user").value || S.users[0]?.name;
}

function idQuery(user) {
  const p = new URLSearchParams();
  if (S.auth !== "jwt") {
    p.set("business_line", S.bl);
    p.set("end_user", user);
  }
  return p;
}

const QUERIES = {
  session: (u) => S.sid && `/v1/sessions/${S.sid}`,
  nodes: (u) => `/v1/nodes?${idQuery(u)}`,
  sessions: (u) => `/v1/sessions?${idQuery(u)}`,
  events: (u) => S.sid && `/v1/sessions/${S.sid}/events?after=0&limit=1000`,
  artifacts: (u) => S.sid && `/v1/sessions/${S.sid}/artifacts`,
  memories: (u) => `/v1/memories?${idQuery(u)}`,
  quota: (u) => `/v1/quota?${idQuery(u)}`,
  usage: (u) => {
    const p = idQuery(u);
    p.set("group_by", "day");
    return `/v1/usage?${p}`;
  },
};
let lastQuery = "session";
for (const b of document.querySelectorAll("#tab-state [data-q]")) b.onclick = () => query(b.dataset.q);

async function query(q) {
  lastQuery = q;
  const user = q === "session" || q === "events" || q === "artifacts" ? ownerUser() : $("st-user").value;
  const path = QUERIES[q](user);
  if (!path) {
    $("st-detail").textContent = "先打开一个 Session";
    return;
  }
  const r = await api("GET", path, user);
  $("st-detail").textContent = `GET ${path}\nHTTP ${r.status}\n\n${typeof r.json === "string" ? r.json : JSON.stringify(r.json, null, 2)}`;
  $("st-summary").replaceChildren(stateSummary(q, r));
}

function stateSummary(q, r) {
  const j = r.json;
  if (r.status !== 200 || !j || typeof j !== "object") return el("div", { class: "line" }, `HTTP ${r.status}`);
  const table = (head, rows) =>
    el(
      "table",
      { class: "summary-table" },
      el("tr", {}, ...head.map((h) => el("th", {}, h))),
      ...rows.map((cells) => el("tr", {}, ...cells.map((c) => el("td", {}, c)))),
    );
  if (q === "session") {
    return el(
      "div",
      {},
      el(
        "div",
        { class: "line" },
        `${j.session_id} · ${j.business_line}/${j.end_user} · ${j.agent?.name}@${j.agent?.version} · seq ${j.seq}`,
      ),
      table(
        ["Run", "状态", "Attempt", "轮次", "在等"],
        (j.runs ?? []).map((x) => [
          short(x.run_id),
          el("span", { class: `badge ${x.status}` }, x.status),
          String(x.attempt),
          String(x.turns),
          waitingText(x.waiting),
        ]),
      ),
    );
  }
  if (q === "nodes") {
    return table(
      ["标签", "类型", "在线", "能力", "Node ID"],
      (j.nodes ?? []).map((n) => [n.label, n.kind, n.online ? "在线" : "离线", n.capabilities.join(", "), n.node_id]),
    );
  }
  if (q === "sessions") {
    return table(
      ["Session", "Agent", "最近输入", ""],
      (j.sessions ?? []).map((s) => {
        const b = el("button", { class: "tiny" }, "打开");
        b.onclick = () => openSession(s.session_id);
        return [s.session_id, s.agent, s.last_input_at ?? "", b];
      }),
    );
  }
  return el("div", { class: "line" }, `HTTP ${r.status}：详情见右侧`);
}

function waitingText(w) {
  if (!w) return "";
  const parts = [w.kind, w.capability, w.node_id];
  if (w.node_online !== undefined) parts.push(w.node_online ? "设备在线" : "设备离线");
  if (w.summary) parts.push(w.summary);
  return parts.filter(Boolean).join(" · ");
}

let autoTimer = null;
$("st-auto").onchange = () => {
  clearInterval(autoTimer);
  if ($("st-auto").checked) autoTimer = setInterval(() => !$("tab-state").hidden && query(lastQuery), 2000);
};

// Session 条：投影摘要，事件到达后刷新。
let projTimer = null;
function scheduleProjection() {
  clearTimeout(projTimer);
  projTimer = setTimeout(refreshProjection, 250);
}

async function refreshProjection() {
  const bar = $("session-bar");
  if (!S.sid) {
    bar.textContent = "没有打开的 Session：新建一个，或在左侧输入 ID 后打开。";
    return;
  }
  const r = await api("GET", `/v1/sessions/${S.sid}`, ownerUser());
  const parts = [el("span", {}, "Session ", el("code", {}, S.sid))];
  if (r.status === 200) {
    const j = r.json;
    parts.push(el("span", {}, `${j.end_user} · ${j.agent?.name}@${j.agent?.version} · seq ${j.seq}`));
    const last = (j.runs ?? []).at(-1);
    if (last) {
      parts.push(el("span", { class: `badge ${last.status}` }, `${short(last.run_id)} ${last.status}`));
      if (last.waiting) parts.push(el("span", { class: "muted" }, `在等：${waitingText(last.waiting)}`));
    }
  } else {
    parts.push(el("span", { class: "muted" }, `投影：HTTP ${r.status}`));
  }
  bar.replaceChildren(...parts);
  if (!$("tab-state").hidden && lastQuery === "session") query("session");
}

// —— 检查器：验证 ——

const results = new Map();
function renderVerify() {
  $("vf-list").replaceChildren(
    ...SCENARIOS.map((s) => {
      const r = results.get(s.id);
      const status = r?.running ? "running" : r ? (r.ok ? "pass" : "fail") : "";
      const icon = { running: "…", pass: "✓", fail: "✗", "": "○" }[status];
      const run = el("button", { class: "tiny", disabled: !!r?.running }, "运行");
      run.onclick = () => verify([s]);
      const open = el("button", { class: "tiny", hidden: !r?.sessionId }, "查看事件");
      open.onclick = () => {
        openSession(r.sessionId);
        setTab("events");
        fetchHTTPEvents();
      };
      const box = el(
        "div",
        { class: "scenario" },
        el(
          "div",
          { class: "row" },
          el("span", { class: `status ${status}` }, icon),
          el("span", { class: "title" }, s.title),
          el("span", { class: "muted" }, s.id),
          el("span", { class: "spacer" }),
          el("span", { class: "muted" }, r && !r.running ? `${r.ms} ms` : ""),
          open,
          run,
        ),
        el("div", { class: "desc" }, s.description),
      );
      if (r?.log.length) {
        const details = el(
          "details",
          { open: !r.ok || r.running },
          el("summary", { class: "muted" }, "步骤"),
          el("pre", {}, r.log.join("\n")),
        );
        box.append(details);
      }
      return box;
    }),
  );
  const done = [...results.values()].filter((r) => !r.running);
  $("vf-summary").textContent = done.length ? `${done.filter((r) => r.ok).length} / ${done.length} 通过` : "";
}

function verifyEnv() {
  if (S.auth === "jwt") {
    const users = S.users.filter((u) => u.token);
    if (users.length < 2) throw new Error("jwt 模式下需要两个带令牌的 EndUser（第二个用于隔离检查）");
    return { users: users.slice(0, 2).map((u) => ({ endUser: u.name, token: u.token })) };
  }
  // 每次运行使用新的 EndUser：设备与 Session 互不干扰，新登记的设备立即出现在工具中。
  const tag = randomHex(3);
  return {
    users: [
      { endUser: `qa-${tag}-a`, token: "" },
      { endUser: `qa-${tag}-b`, token: "" },
    ],
  };
}

async function verify(list) {
  let ids;
  try {
    ids = verifyEnv();
  } catch (e) {
    $("vf-summary").textContent = e.message;
    return;
  }
  const env = { wsUrl: WS_URL, httpBase: HTTP, businessLine: S.auth === "jwt" ? "" : S.bl, agent: "console", ...ids };
  for (const s of list) {
    const r = { running: true, ok: false, ms: 0, log: [], sessionId: "" };
    results.set(s.id, r);
    renderVerify();
    const out = await runScenario(s, env, (m) => {
      r.log.push(m);
      renderVerify();
    });
    Object.assign(r, out, { running: false });
    renderVerify();
  }
}
$("vf-all").onclick = () => verify(SCENARIOS);

// —— Session 操作 ——

$("create").onclick = async () => {
  const user = $("new-user").value || S.users[0]?.name;
  const body = { agent: S.agent };
  if (S.auth !== "jwt") Object.assign(body, { business_line: S.bl, end_user: user });
  const r = await api("POST", "/v1/sessions", user, body);
  if (r.status !== 201) return alert(`创建失败：HTTP ${r.status} ${JSON.stringify(r.json)}`);
  openSession(r.json.session_id);
};
$("open").onclick = () => openSession($("sid").value);
$("close-session").onclick = async () => {
  if (!S.sid || !confirm("关闭后 Session 不再接收输入（不可逆）。继续？")) return;
  const r = await api("POST", `/v1/sessions/${S.sid}/close`, ownerUser(), { reason: "console" });
  if (r.status >= 300) alert(`关闭失败：HTTP ${r.status} ${JSON.stringify(r.json)}`);
};
$("list-sessions").onclick = async () => {
  const user = $("new-user").value || S.users[0]?.name;
  const r = await api("GET", `/v1/sessions?${idQuery(user)}`, user);
  $("sessions").replaceChildren(
    ...(r.json?.sessions ?? []).slice(0, 12).map((s) => {
      const row = el(
        "div",
        { class: "session-row", title: `${s.agent} · ${s.last_input_at ?? ""}` },
        `${s.session_id}${s.closed_at ? "（已关闭）" : ""}`,
      );
      row.onclick = () => openSession(s.session_id);
      return row;
    }),
  );
  if (r.status !== 200) $("sessions").textContent = `HTTP ${r.status}`;
};

$("demo").onclick = async () => {
  const user = $("new-user").value || S.users[0]?.name || "u1";
  const want = ["phone", "desktop", "web"];
  for (const preset of want) {
    if ([...devices.values()].some((d) => d.rec.preset === preset && d.rec.user === user)) continue;
    const rec = newRecord(preset, user);
    S.records.push(rec);
    persist();
    addDevice(rec);
  }
  const mine = [...devices.values()].filter((d) => d.rec.user === user);
  for (const d of mine) {
    if (!d.dev.wanted) {
      d.dev.connect();
      d.rec.wanted = true;
    }
  }
  persist();
  try {
    await Promise.all(mine.map((d) => d.dev.ready()));
  } catch (e) {
    return alert(`设备连接失败：${e.message}`);
  }
  $("create").click();
  const phone = mine.find((d) => d.rec.preset === "phone");
  const pc = mine.find((d) => d.rec.preset === "desktop");
  if (phone && pc) {
    phone.ui.focus.checked = true;
    phone.ui.input.value = `call ${pc.dev.label}__write_file {"path":"plan.txt","content":"周五发布"}`;
    const web = mine.find((d) => d.rec.preset === "web");
    if (web) web.ui.input.value = "ask 周会改到哪天？| 周四 | 周五";
    pc.ui.input.value = `call ${phone.dev.label}__get_location`;
  }
};

// —— 标签页与布局 ——

function setTab(name) {
  for (const b of document.querySelectorAll("#tabs button")) b.classList.toggle("active", b.dataset.tab === name);
  for (const t of document.querySelectorAll(".tab")) t.hidden = t.id !== `tab-${name}`;
  if (name === "frames") scheduleFrames();
  if (name === "events") scheduleEvents();
  if (name === "verify") renderVerify();
  if (name === "state") query(lastQuery);
}
for (const b of document.querySelectorAll("#tabs button")) b.onclick = () => setTab(b.dataset.tab);

if (S.inspector) document.documentElement.style.setProperty("--inspector", S.inspector);
$("splitter").addEventListener("mousedown", (e) => {
  e.preventDefault();
  const move = (ev) => {
    const h = Math.max(120, Math.min(window.innerHeight - 200, window.innerHeight - ev.clientY));
    S.inspector = `${h}px`;
    document.documentElement.style.setProperty("--inspector", S.inspector);
  };
  const up = () => {
    window.removeEventListener("mousemove", move);
    window.removeEventListener("mouseup", up);
    persist();
  };
  window.addEventListener("mousemove", move);
  window.addEventListener("mouseup", up);
});

// —— 启动 ——

renderUsers();
$("sid").value = S.sid;
// 设备加入时订阅当前 Session（addDevice → openOn）。
for (const rec of S.records) addDevice(rec);
refreshProjection();
if (devices.size) select([...devices.keys()][0]);
renderVerify();
