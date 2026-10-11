// 调试控制台（docs/design/console.md）：三个视图共用一组模拟设备与当前 Session。
//   对话     一台设备（默认是网页客户端）全屏展示聊天：语音、图片与文件、卡片、审批与提问、通话。
//   设备     每个 EndUser 的手机、电脑、手表、音箱、车机与网页：在线 / 离线 / 断网 / 崩溃，模拟应用的状态，能力的执行方式，
//           调用账本；每台设备有自己的迷你对话，同一 Session 多端同步。
//   控制台   时间线、事件、帧、状态、故障注入、指标与验证场景。
// 设备与设置保存在浏览器的 localStorage 中：刷新后设备 ID 不变，在网关看来是同一台设备重新上线。

import { ChatPanel } from "./chat.js";
import { DEVICE_PRESETS, SessionView, SimDevice, payloadOf, randomHex } from "./device.js";
import { DeviceTile } from "./devices.js";
import { Inspector, summarize } from "./inspector.js";
import { runScenario } from "./scenarios.js";
import { $, Artifacts, h, hue, short, toast, fill } from "./ui.js";

const WS_URL = `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/v1/connect`;
const HTTP = location.origin;
const SAVED = "yanshi.console.v2";

const AGENTS = [
  ["concierge", "总管：委托日程 / 出行 / 研究专员（真实模型）"],
  ["assistant", "通用助手（真实模型，能看图、听语音）"],
  ["schedule", "日程管家（专员）"],
  ["travel", "出行助手（专员）"],
  ["research", "研究员（专员，需要沙箱）"],
  ["console", "脚本化模型：call / ask / stream 命令，无需密钥"],
  ["echo", "复述"],
];

// —— 设置 ——

const saved = JSON.parse(localStorage.getItem(SAVED) ?? "null") ?? {};
const S = {
  bl: saved.bl ?? "demo",
  auth: saved.auth ?? "none",
  users: saved.users ?? [{ name: "alice", token: "" }],
  user: saved.user ?? "alice",
  agent: saved.agent ?? "concierge",
  sid: saved.sid ?? "",
  view: saved.view ?? "chat",
  devices: saved.devices ?? [],
  chatDevice: saved.chatDevice ?? {},
};

function persist() {
  for (const d of devices.values()) {
    d.rec.online = d.sim.wanted;
    d.rec.modes = Object.fromEntries(d.sim.capabilities.map((c) => [c.name, { mode: c.mode, delayMs: c.delayMs }]));
  }
  S.devices = [...devices.values()].map((d) => d.rec);
  localStorage.setItem(SAVED, JSON.stringify(S));
}

// 链接带入令牌：/console#token.<用户>=<令牌>（片段不随请求发给服务端，读入后从地址栏清除）。
if (location.hash.includes("token.")) {
  for (const kv of location.hash.slice(1).split("&")) {
    const [k, v] = kv.split("=");
    if (!k?.startsWith("token.") || !v) continue;
    const name = decodeURIComponent(k.slice(6));
    const u = S.users.find((x) => x.name === name);
    if (u) u.token = decodeURIComponent(v);
    else S.users.push({ name, token: decodeURIComponent(v) });
    S.auth = "jwt";
  }
  history.replaceState(null, "", location.pathname);
}

const tokenOf = (user) => (S.auth === "jwt" ? (S.users.find((u) => u.name === user)?.token ?? "") : "");
const artifacts = new Artifacts(HTTP, tokenOf);

// —— 设备 ——

/** id → {rec, sim, views: Map<sid, SessionView>} */
const devices = new Map();
const frames = [];
const frameListeners = new Set();
const MAX_FRAMES = 4000;

function newRecord(preset, user) {
  const p = DEVICE_PRESETS[preset];
  const n = [...devices.values()].filter((d) => d.rec.user === user && d.rec.preset === preset).length;
  return { id: `console-${user}-${preset}-${randomHex(3)}`, preset, user, label: n ? `${p.label}${n + 1}` : p.label, online: true };
}

function addDevice(rec) {
  const p = DEVICE_PRESETS[rec.preset] ?? DEVICE_PRESETS.web;
  const caps = p.capabilities.map((name) => ({ name, ...(rec.modes?.[name] ?? {}) }));
  const sim = new SimDevice({
    url: WS_URL,
    httpBase: HTTP,
    deviceId: rec.id,
    label: rec.label,
    kind: p.kind,
    businessLine: S.auth === "none" ? S.bl : "",
    endUser: S.auth === "none" ? rec.user : "",
    token: tokenOf(rec.user),
    clientOnly: !!p.clientOnly,
    capabilities: caps,
  });
  const entry = { rec, sim, views: new Map() };
  devices.set(rec.id, entry);
  sim.on((n) => onNotice(entry, n));
  if (rec.online !== false) sim.connect();
  return entry;
}

function removeDevice(entry) {
  for (const v of entry.views.values()) v.dispose();
  entry.sim.disconnect();
  devices.delete(entry.rec.id);
  if (S.chatDevice[entry.rec.user] === entry.rec.id) delete S.chatDevice[entry.rec.user];
  persist();
  renderAll();
}

function onNotice(entry, n) {
  if (n.type === "frame") {
    if (n.msg?.call_audio) return;
    const kind = n.msg ? Object.keys(n.msg)[0] : "raw";
    frames.push({
      device: entry.rec.id,
      label: entry.sim.label || entry.rec.label,
      dir: n.dir,
      kind: kind === "request" ? `request.${Object.keys(n.msg.request).find((k) => k !== "request_id")}` : kind,
      summary: frameSummary(n.msg),
      msg: n.msg,
      raw: n.raw,
      at: Date.now(),
    });
    if (frames.length > MAX_FRAMES) frames.splice(0, frames.length - MAX_FRAMES);
    for (const fn of frameListeners) fn();
  } else if (n.type === "status") {
    if (entry.sim.online) ensureSubscribed(entry);
    renderTop();
    if (S.view === "chat") scheduleSide();
  }
}

function frameSummary(m) {
  if (!m) return "";
  if (m.event) {
    const [k, p] = payloadOf(m.event);
    return `#${m.event.seq} ${k} ${summarize(k, p) ?? ""}`.slice(0, 160);
  }
  if (m.delta) return `${m.delta.snapshot ? "snapshot " : ""}${m.delta.end ? "end " : ""}${(m.delta.text ?? "").slice(0, 60)}`;
  if (m.invoke) return `${m.invoke.capability} ${m.invoke.call_id}`;
  if (m.result) return `${m.result.call_id}${m.result.is_error ? " ✗" : ""}`;
  if (m.response) return m.response.error ? `${m.response.request_id} ✗ ${m.response.error.code}` : `${m.response.request_id} ok`;
  if (m.request) return m.request.request_id;
  if (m.call_event)
    return Object.keys(m.call_event)
      .filter((k) => k !== "call_id")
      .join(",");
  return JSON.stringify(m).slice(0, 120);
}

/** 当前 EndUser 的设备。 */
const userDevices = () => [...devices.values()].filter((d) => d.rec.user === S.user);

/** "对话"视图所用的设备：用户选定的，默认是网页客户端（没有时自动添加一台）。 */
function chatEntry() {
  let e = devices.get(S.chatDevice[S.user]);
  if (!e || e.rec.user !== S.user) e = userDevices().find((d) => d.rec.preset === "web");
  if (!e) {
    e = addDevice({ ...newRecord("web", S.user), label: "web" });
    persist();
  }
  S.chatDevice[S.user] = e.rec.id;
  return e;
}

// —— Session ——

/** 设备上 sid 的视图：首次使用时订阅（重连后 SimDevice 自动续订）。 */
function viewOf(entry, sid) {
  if (!sid) return undefined;
  let v = entry.views.get(sid);
  if (!v) {
    v = new SessionView(entry.sim, sid);
    entry.views.set(sid, v);
    entry.sim.subscribe(sid, 0n);
    v.on((kind) => onViewChange(entry, v, kind));
  }
  return v;
}

function ensureSubscribed(entry) {
  if (S.sid && entry.rec.user === S.user) viewOf(entry, S.sid);
}

/** 子 Session 的视图（委托，docs/design/m5-multi-agent.md）：在"对话"设备上订阅，供时间线与调用条展示。 */
function childView(sid) {
  return viewOf(chatEntry(), sid);
}

let inspectQueued = false;
function onViewChange(entry, v, kind) {
  // 发现新的委托：订阅子 Session（递归：子 Session 的视图同样经过这里）。
  if (kind === "event" && entry === devices.get(S.chatDevice[S.user])) {
    for (const d of v.delegations()) if (!entry.views.has(d.childSessionId)) viewOf(entry, d.childSessionId);
  }
  if (kind === "ended" && v.sessionId === S.sid && v.ended) toast(`订阅结束：${v.ended}`, "error");
  if (S.view === "console" && !inspectQueued) {
    inspectQueued = true;
    setTimeout(() => ((inspectQueued = false), inspector.schedule()), 150);
  }
  if (S.view === "chat" && kind === "event") scheduleSide();
}

function openSession(sid) {
  S.sid = sid;
  persist();
  for (const d of userDevices()) ensureSubscribed(d);
  chat?.dispose();
  chat = null;
  for (const t of tiles.values()) t.sessionChanged();
  inspector.stateResult = null;
  renderAll();
}

async function newSession() {
  const e = chatEntry();
  try {
    await e.sim.ready(5000);
    const sid = await e.sim.createSession(S.agent);
    openSession(sid);
    toast(`已新建 Session（${S.agent}）`);
    refreshSessions();
  } catch (err) {
    toast(`新建失败：${err.message}`, "error");
  }
}

let sessions = [];
async function refreshSessions() {
  const r = await http("GET", `/v1/sessions?${idQuery()}&limit=30`).catch(() => ({ status: 0 }));
  sessions = r.status === 200 ? (r.json.sessions ?? []) : [];
  renderTop();
  if (S.view === "chat") scheduleSide();
}

// —— HTTP ——

function idQuery(user = S.user) {
  return S.auth === "none" ? `business_line=${encodeURIComponent(S.bl)}&end_user=${encodeURIComponent(user)}` : "";
}

async function http(method, path, body, user = S.user) {
  const headers = { "Content-Type": "application/json" };
  const tok = tokenOf(user);
  if (tok) headers.Authorization = `Bearer ${tok}`;
  const res = await fetch(HTTP + path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  const text = await res.text();
  let json;
  try {
    json = text ? JSON.parse(text) : undefined;
  } catch {
    json = text;
  }
  return { status: res.status, json };
}

// —— 顶栏 ——

function renderTop() {
  const live = userDevices();
  const online = live.filter((d) => d.sim.online).length;
  fill(
    $("top"),
    h("b", { class: "brand" }, "yanshi"),
    h(
      "nav",
      { class: "views" },
      [
        ["chat", "💬 对话"],
        ["devices", "📱 设备"],
        ["console", "🛠 控制台"],
      ].map(([id, name]) => h("button", { class: S.view === id ? "active" : "", onclick: () => setView(id) }, name)),
    ),
    h("span", { class: "sep" }),
    h(
      "label",
      { class: "field" },
      "用户",
      h(
        "select",
        { onchange: (e) => (e.target.value === "+" ? addUser() : switchUser(e.target.value)) },
        S.users.map((u) => h("option", { value: u.name, selected: u.name === S.user }, u.name)),
        h("option", { value: "+" }, "+ 添加用户…"),
      ),
    ),
    h(
      "label",
      { class: "field" },
      "Agent",
      h(
        "select",
        { title: "新建 Session 时使用", onchange: (e) => ((S.agent = e.target.value), persist(), scheduleSide()) },
        AGENTS.map(([id, desc]) => h("option", { value: id, selected: id === S.agent, title: desc }, id)),
        AGENTS.some(([id]) => id === S.agent) ? null : h("option", { value: S.agent, selected: true }, S.agent),
      ),
    ),
    h(
      "label",
      { class: "field grow" },
      "Session",
      h(
        "select",
        { onchange: (e) => e.target.value && openSession(e.target.value), onfocus: refreshSessions },
        h("option", { value: "" }, S.sid ? "" : "（未打开）"),
        sessionOptions(),
      ),
    ),
    h("button", { class: "primary", onclick: newSession }, "+ 新建"),
    h("span", { class: "spacer" }),
    h("span", { class: `gw ${online ? "ok" : "bad"}`, title: "当前用户在线的设备" }, `${online}/${live.length} 台设备在线`),
    h("button", { class: "ghost", title: "设置", onclick: () => $("settings").showModal() }, "⚙"),
  );
}

function sessionOptions() {
  const list = [...sessions];
  if (S.sid && !list.some((s) => s.session_id === S.sid)) list.unshift({ session_id: S.sid, agent: "" });
  return list.map((s) =>
    h(
      "option",
      { value: s.session_id, selected: s.session_id === S.sid },
      `${short(s.session_id)} · ${s.agent || "?"}${s.closed_at ? " · 已关闭" : ""}`,
    ),
  );
}

function addUser() {
  const name = prompt("EndUser 名称（-auth jwt 时用 yanshi token -key demo.key -user <名称> 签发令牌）");
  if (!name) return renderTop();
  if (!S.users.some((u) => u.name === name)) S.users.push({ name, token: "" });
  switchUser(name);
}

function switchUser(name) {
  S.user = name;
  S.sid = "";
  persist();
  chat?.dispose();
  chat = null;
  refreshSessions();
  renderAll();
}

function setView(v) {
  S.view = v;
  persist();
  renderAll();
}

// —— 对话视图 ——

let chat = null;
let sideQueued = false;
function scheduleSide() {
  if (sideQueued) return;
  sideQueued = true;
  setTimeout(() => ((sideQueued = false), S.view === "chat" && renderSide()), 200);
}

function renderChat() {
  const e = chatEntry();
  const view = viewOf(e, S.sid);
  if (!view) {
    chat?.dispose();
    chat = null;
    fill(
      $("chat-main"),
      h(
        "div",
        { class: "welcome" },
        h("h2", {}, "开始一段对话"),
        h(
          "p",
          {},
          "选择 Agent 后新建 Session。推荐 ",
          h("b", {}, "concierge"),
          "：它把闹钟、导航、查资料分别委托给专员，在你的模拟设备上执行，再用卡片汇总结果。",
        ),
        h(
          "div",
          { class: "row" },
          h("button", { class: "primary big", onclick: newSession }, `新建 Session（${S.agent}）`),
          h("button", { class: "big", onclick: () => setupScene() }, "一键搭建设备：手机 + 电脑 + 手表 + 音箱 + 车机"),
        ),
        h("p", { class: "muted" }, '没有模型密钥时选 console（脚本化模型）：输入 call iphone__set_alarm {"time":"07:00"} 这样的命令。'),
      ),
    );
  } else if (!chat || chat.view !== view) {
    chat?.dispose();
    chat = new ChatPanel({ device: e.sim, view, ctx: { artifacts, user: S.user, children: childView } });
    e.sim.setActivity(S.sid, true);
    fill($("chat-main"), chat.root);
  }
  renderSide();
}

const EXAMPLES = {
  concierge: [
    "明早 7 点叫我起床，顺便导航去公司（望京 SOHO）",
    "我到家前把车里空调开到 22 度，再放点轻音乐",
    "帮我看看电脑上 budget.csv 里哪个月结余最多",
    "这周六下午 3 点和老王打球，帮我记到日历里",
  ],
  assistant: ["明早 7 点叫我起床", "帮我拍张照片看看桌上有什么", "看一下电脑上的 notes.txt", "记住：我对花生过敏"],
  console: [
    'call iphone__set_alarm {"time":"07:00","label":"晨跑"}',
    'call show_card {"title":"天气","image":"icon:weather","body":[{"type":"facts","facts":[{"label":"今天","value":"晴 25℃"}]}],"buttons":[{"id":"more","label":"看明天"}]}',
    "ask 选哪个？ | 甲 | 乙",
    'call agent__console-helper {"task":"复述这句话"}',
    "stream 100",
  ],
};

function renderSide() {
  const e = devices.get(S.chatDevice[S.user]);
  const view = e && S.sid ? e.views.get(S.sid) : undefined;
  const others = userDevices().filter((d) => !d.sim.cfg.clientOnly);
  const agent = S.sid ? (sessions.find((s) => s.session_id === S.sid)?.agent ?? S.agent) : S.agent;
  fill(
    $("chat-side"),
    h(
      "section",
      {},
      h("h4", {}, "对话所用设备"),
      h(
        "select",
        {
          onchange: (ev) => {
            S.chatDevice[S.user] = ev.target.value;
            persist();
            chat?.dispose();
            chat = null;
            renderChat();
          },
        },
        userDevices().map((d) =>
          h("option", { value: d.rec.id, selected: d === e }, `${DEVICE_PRESETS[d.rec.preset]?.icon ?? ""} ${d.sim.label || d.rec.label}`),
        ),
      ),
      h("p", { class: "muted" }, "从哪台设备发消息：输入会注明这台设备（设备歧义消解）。"),
    ),
    h(
      "section",
      {},
      h("h4", {}, "设备 ", h("button", { class: "tiny", onclick: () => setView("devices") }, "管理")),
      others.length
        ? others.map((d) =>
            h(
              "div",
              { class: "side-dev" },
              h("span", { class: `dot ${d.sim.status}` }),
              DEVICE_PRESETS[d.rec.preset]?.icon,
              " ",
              d.sim.label || d.rec.label,
              h("span", { class: "spacer" }),
              h(
                "button",
                {
                  class: "tiny",
                  onclick: () => {
                    d.sim.wanted ? d.sim.disconnect() : d.sim.connect();
                    persist();
                    setTimeout(renderSide, 50);
                  },
                },
                d.sim.wanted ? "下线" : "上线",
              ),
            ),
          )
        : h(
            "div",
            {},
            h("p", { class: "muted" }, "还没有可以执行能力的设备。"),
            h("button", { class: "small primary", onclick: () => setupScene() }, "一键搭建"),
          ),
    ),
    view ? h("section", {}, h("h4", {}, "委托"), delegationTree(view)) : null,
    h(
      "section",
      {},
      h("h4", {}, "试试"),
      (EXAMPLES[agent] ?? EXAMPLES.assistant).map((t) =>
        h(
          "button",
          {
            class: "example",
            onclick: () => {
              if (!chat) return;
              chat.input.value = t;
              chat.input.focus();
            },
          },
          t,
        ),
      ),
    ),
  );
}

function delegationTree(view, depth = 0) {
  const list = view.delegations();
  if (!list.length) return depth ? null : h("div", { class: "muted" }, "本 Session 还没有委托");
  return h(
    "ul",
    { class: "tree" },
    list.map((d) => {
      const child = childView(d.childSessionId);
      const last = child?.payloads("run_requested").at(-1);
      const end = last && child.runEnd(last.run_id);
      return h(
        "li",
        {},
        h(
          "span",
          { class: `pill ${end?.kind ?? "running"}` },
          end ? { run_completed: "完成", run_failed: "失败", run_interrupted: "中断" }[end.kind] : "进行中",
        ),
        " 🤝 ",
        h("b", {}, d.agent),
        h("code", { class: "muted", title: d.childSessionId }, " ", short(d.childSessionId)),
        child ? delegationTree(child, depth + 1) : null,
      );
    }),
  );
}

// —— 设备视图 ——

const tiles = new Map();

function renderDevices() {
  const list = userDevices();
  for (const [id, t] of tiles) {
    if (!devices.has(id) || devices.get(id).rec.user !== S.user) {
      t.dispose();
      tiles.delete(id);
    }
  }
  const grid = list.map((d) => {
    let t = tiles.get(d.rec.id);
    if (!t) {
      t = new DeviceTile(d, {
        artifacts,
        user: S.user,
        view: (entry) => viewOf(entry, S.sid),
        children: childView,
        onRemove: removeDevice,
        onChange: persist,
        save: persist,
      });
      tiles.set(d.rec.id, t);
    }
    return t.root;
  });
  fill(
    $("devices-main"),
    h(
      "div",
      { class: "toolbar" },
      h("b", {}, `${S.user} 的设备`),
      Object.entries(DEVICE_PRESETS).map(([k, p]) =>
        h(
          "button",
          {
            class: "small",
            onclick: () => {
              const e = addDevice(newRecord(k, S.user));
              ensureSubscribed(e);
              persist();
              renderDevices();
            },
          },
          `+ ${p.icon} ${p.title}`,
        ),
      ),
      h("span", { class: "spacer" }),
      h("button", { class: "small primary", onclick: () => setupScene() }, "一键搭建全套"),
    ),
    h(
      "p",
      { class: "muted hint" },
      "下线：Run 挂起等待设备上线；断网：自动重连、续订、重发未确认的结果；崩溃：执行中的调用留下记录，重新投递后按 call_id 去重。",
    ),
    h("div", { class: "grid" }, grid.length ? grid : h("div", { class: "empty big" }, "还没有设备。添加一台，或一键搭建全套。")),
  );
}

function setupScene() {
  const have = new Set(userDevices().map((d) => d.rec.preset));
  const labels = { phone: "iphone", desktop: "macbook", watch: "watch", speaker: "speaker", car: "car", web: "web" };
  for (const k of ["web", "phone", "desktop", "watch", "speaker", "car"]) {
    if (!have.has(k)) addDevice({ ...newRecord(k, S.user), label: labels[k] });
  }
  persist();
  if (S.sid) for (const d of userDevices()) ensureSubscribed(d);
  toast("已添加：网页、手机、电脑、手表、音箱、车机。新登记的设备至多 10 秒后出现在 Agent 的工具中。");
  renderAll();
}

// —— 控制台视图 ——

const inspector = new Inspector($("console-main"), {
  sessionId: () => S.sid,
  observer: () => {
    const e = chatEntry();
    return { device: e.sim, view: viewOf(e, S.sid) };
  },
  childView,
  devices: () => [...devices.values()],
  views: () =>
    userDevices()
      .filter((d) => d.views.has(S.sid))
      .map((d) => ({ label: d.sim.label || d.rec.label, view: d.views.get(S.sid) })),
  frames,
  onFrames: (fn) => frameListeners.add(fn),
  http: (method, path, body) => http(method, path, body),
  idQuery: () => idQuery(),
  verifyEnv,
  runScenario,
  artifacts,
  user: () => S.user,
});

function verifyEnv() {
  const users =
    S.auth === "jwt"
      ? S.users
          .filter((u) => u.token)
          .slice(0, 2)
          .map((u) => ({ endUser: u.name, token: u.token }))
      : [
          { endUser: `qa-${randomHex(3)}-a`, token: "" },
          { endUser: `qa-${randomHex(3)}-b`, token: "" },
        ];
  while (users.length < 2) users.push({ endUser: "", token: "" });
  return { wsUrl: WS_URL, httpBase: HTTP, businessLine: S.auth === "none" ? S.bl : "", agent: "console", helper: "console-helper", users };
}

// —— 设置对话框 ——

function renderSettings() {
  const dlg = $("settings");
  fill(
    dlg,
    h(
      "form",
      { method: "dialog" },
      h("h3", {}, "设置"),
      h(
        "label",
        { class: "field" },
        "鉴权",
        h(
          "select",
          {
            onchange: (e) => {
              S.auth = e.target.value;
              persist();
              renderSettings();
            },
          },
          h("option", { value: "none", selected: S.auth === "none" }, "自报身份（serve -auth none，只允许回环地址）"),
          h("option", { value: "jwt", selected: S.auth === "jwt" }, "令牌（serve -auth jwt）"),
        ),
      ),
      S.auth === "none"
        ? h("label", { class: "field" }, "业务线", h("input", { value: S.bl, onchange: (e) => ((S.bl = e.target.value), persist()) }))
        : null,
      h("h4", {}, "EndUser"),
      S.users.map((u) =>
        h(
          "div",
          { class: "row" },
          h("b", { class: "user-name", style: { "--h": hue(u.name) } }, u.name),
          S.auth === "jwt"
            ? h("input", {
                class: "grow",
                placeholder: "令牌：yanshi token -key demo.key -user " + u.name,
                value: u.token,
                onchange: (e) => ((u.token = e.target.value.trim()), persist()),
              })
            : null,
          S.users.length > 1
            ? h(
                "button",
                {
                  type: "button",
                  class: "tiny",
                  onclick: () => {
                    S.users = S.users.filter((x) => x !== u);
                    persist();
                    renderSettings();
                  },
                },
                "删除",
              )
            : null,
        ),
      ),
      h("p", { class: "muted" }, '改了鉴权方式、业务线或令牌后点"应用"：设备以新的身份重新连接。'),
      h(
        "div",
        { class: "row" },
        h("button", { type: "button", class: "danger", onclick: resetAll }, "清空全部设备与设置"),
        h("span", { class: "spacer" }),
        h(
          "button",
          {
            type: "button",
            class: "primary",
            onclick: () => {
              persist();
              location.reload();
            },
          },
          "应用",
        ),
        h("button", {}, "关闭"),
      ),
    ),
  );
}

function resetAll() {
  if (!confirm("清空本机保存的设备与设置？（网关中的 Session 与 Node 登记不受影响）")) return;
  for (const d of devices.values()) d.sim.disconnect();
  localStorage.removeItem(SAVED);
  location.reload();
}

// —— 布局 ——

function renderAll() {
  renderTop();
  for (const v of ["chat", "devices", "console"]) $(`view-${v}`).hidden = S.view !== v;
  if (S.view === "chat") renderChat();
  if (S.view === "devices") renderDevices();
  if (S.view === "console") {
    inspector.render();
    inspector.poll();
  }
}

document.addEventListener("visibilitychange", () => {
  const e = devices.get(S.chatDevice[S.user]);
  if (e && S.sid) e.sim.setActivity(S.sid, document.visibilityState === "visible" && S.view === "chat");
});

// —— 启动 ——

for (const rec of S.devices) addDevice(rec);
renderSettings();
if (S.sid) for (const d of userDevices()) ensureSubscribed(d);
renderAll();
refreshSessions();
