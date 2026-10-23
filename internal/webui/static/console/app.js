// 调试控制台（docs/design/console.md）：三个视图共用一组模拟设备与当前 Session。
//   对话     一台设备（默认是网页客户端）全屏展示聊天：语音、图片与文件、卡片、审批与提问、通话。
//   设备     每个 EndUser 的手机、电脑、手表、音箱、车机与网页：在线 / 离线 / 断网 / 崩溃，模拟应用的状态，能力的执行方式，
//           调用账本；每台设备有自己的迷你对话，同一 Session 多端同步。
//   控制台   时间线、事件、帧、状态、故障注入、指标与验证场景。
// 设备与设置保存在浏览器的 localStorage 中：刷新后设备 ID 不变，在网关看来是同一台设备重新上线。

import { ChatPanel } from "./chat.js";
import { renderDrawer as drawerContent } from "./drawer.js";
import { DEVICE_PRESETS, SessionView, SimDevice, payloadOf, randomHex, textOf } from "./device.js";
import { DeviceTile } from "./devices.js";
import { Inspector, summarize } from "./inspector.js";
import { runScenario } from "./scenarios.js";
import { XinliView } from "./xinli.js";
import { $, Artifacts, ago, fill, h, hue, toast } from "./ui.js";

const WS_URL = `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}/v1/connect`;
const HTTP = location.origin;
const SAVED = "yanshi.console.v2";

const AGENTS = [
  ["concierge", "总管：委托日程 / 出行 / 研究专员（真实模型）"],
  ["assistant", "通用助手（真实模型，能看图、听语音）"],
  ["schedule", "日程管家（专员）"],
  ["travel", "出行助手（专员）"],
  ["research", "研究员（专员，需要沙箱）"],
  ["xinli-day", "旁听的每日整理：🎧 提交转写后发「整理今天」（真实模型，需 serve -xinli-mock）"],
  ["xinli-assistant", "旁听业务线的助手：查项目与最近几天（真实模型，需 serve -xinli-mock）"],
  ["console", "脚本化模型：call / ask / stream 命令，无需密钥"],
  ["console-capture", "脚本化模型，开启旁听：🎧 提交转写后 call captures {}"],
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
  drawer: saved.drawer ?? true,
  captureOpen: saved.captureOpen ?? false,
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
  if (foreground?.entry === entry) foreground = null;
  for (const v of entry.views.values()) (clearTimeout(v.retry.timer), v.dispose());
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

/**
 * 设备上 sid 的视图：首次使用时订阅（重连后 SimDevice 自动续订）。订阅已被网关结束的视图不算有效订阅：可以再订阅时
 * （resubscribable）从已收到的位置重订，按退避限制频率，免得每次重绘都订阅一次。
 */
function viewOf(entry, sid) {
  if (!sid) return undefined;
  let v = entry.views.get(sid);
  if (!v) {
    v = new SessionView(entry.sim, sid);
    // 订阅被结束后重订的退避：backoff 毫秒，at 之前不重订，timer 为委托进行中时的定时重订。
    v.retry = { backoff: 0, at: 0, timer: null };
    entry.views.set(sid, v);
    entry.sim.subscribe(sid, 0n);
    v.on((kind) => onViewChange(entry, v, kind));
  } else if (!entry.sim.subs.has(sid) && !v.retry.timer && Date.now() >= v.retry.at && resubscribable(entry, v)) {
    resubscribe(entry, v);
  }
  return v;
}

function resubscribe(entry, v) {
  v.ended = "";
  entry.sim.subscribe(v.sessionId, v.lastSeq());
}

/**
 * 订阅结束后可以再订阅：limit（订阅数到了网关的上限，退订别的 Session 后有空位），或 not_found、error 而该 Session 是进行中的
 * 委托派生的子 Session：Worker 先提交 ToolCallStarted、再创建子 Session，发现委托立即订阅时它可能还不存在。deleted 不再订阅。
 */
function resubscribable(entry, v) {
  if (v.ended === "limit") return true;
  return (v.ended === "not_found" || v.ended === "error") && delegated(entry, v.sessionId);
}

/** sid 是这台设备上某个视图里进行中的委托派生的子 Session。 */
function delegated(entry, sid) {
  for (const pv of entry.views.values()) if (pv.activeDelegations().some((d) => d.childSessionId === sid)) return true;
  return false;
}

/** 订阅被网关结束：委托仍在进行时按退避（200ms 起翻倍，至多 5 秒）定时重订；其余可重订的原因等下次用到视图时再订。 */
function onEnded(entry, v) {
  const r = v.retry;
  const first = !r.backoff;
  r.backoff = Math.min(r.backoff ? r.backoff * 2 : 200, 5000);
  r.at = Date.now() + r.backoff;
  if (!r.timer && v.ended !== "limit" && resubscribable(entry, v)) {
    r.timer = setTimeout(() => {
      r.timer = null;
      if (entry.views.get(v.sessionId) === v && !entry.sim.subs.has(v.sessionId) && resubscribable(entry, v)) resubscribe(entry, v);
    }, r.backoff);
  }
  if (first && v.sessionId === S.sid && v.ended) toast(`订阅结束：${v.ended}`, "error");
}

/** sid 与它的委托派生的子 Session（递归，按各设备上已有的视图）。 */
function subtree(sid) {
  const out = new Set();
  const walk = (s) => {
    if (!s || out.has(s)) return;
    out.add(s);
    for (const d of devices.values()) for (const c of d.views.get(s)?.delegations() ?? []) walk(c.childSessionId);
  };
  walk(sid);
  return out;
}

/**
 * 退订并丢弃 sid 及其委托派生的子 Session 在所有设备上的视图，keep 中的除外（新打开的 Session 恰是其后代时保留它）。
 * 否则每打开一个 Session 就多几个订阅，很快用完网关对一条 Connection 的订阅上限（channel.MaxSubscriptions）。
 */
function releaseSession(sid, keep = new Set()) {
  for (const s of subtree(sid)) {
    if (keep.has(s)) continue;
    for (const entry of devices.values()) {
      const v = entry.views.get(s);
      if (!v) continue;
      clearTimeout(v.retry.timer);
      v.dispose();
      entry.views.delete(s);
      entry.sim.unsubscribe(s);
    }
  }
}

/**
 * 报告前台状态：只有"对话"视图可见时，它所用的设备把当前 Session 报告为前台；切换 Session、更换对话设备、离开"对话"
 * 视图或页面隐藏时，先撤下之前报告的。notify 据此判断用户是否正在看，决定是否发提醒（internal/notify）。
 */
let foreground = null;
function reportForeground() {
  const entry = S.view === "chat" && S.sid && document.visibilityState === "visible" ? devices.get(S.chatDevice[S.user]) : undefined;
  const want = entry && entry.rec.user === S.user ? { entry, sid: S.sid } : null;
  if (foreground && (want?.entry !== foreground.entry || want?.sid !== foreground.sid)) {
    foreground.entry.sim.setActivity(foreground.sid, false);
    foreground = null;
  }
  // 还没订阅时 setActivity 不起作用：不记下，下次再报告。
  if (want && !foreground && want.entry.sim.subs.has(want.sid)) {
    want.entry.sim.setActivity(want.sid, true);
    foreground = want;
  }
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
  if (kind === "event") v.retry.backoff = 0;
  if (kind === "ended") onEnded(entry, v);
  if (S.view === "console" && !inspectQueued) {
    inspectQueued = true;
    setTimeout(() => ((inspectQueued = false), inspector.schedule()), 150);
  }
  if (S.view === "chat" && kind === "event") scheduleSide();
}

function openSession(sid) {
  selection = null;
  const prev = S.sid;
  S.sid = sid;
  persist();
  if (prev && prev !== sid) releaseSession(prev, subtree(sid));
  for (const d of userDevices()) ensureSubscribed(d);
  chat?.dispose();
  chat = null;
  for (const t of tiles.values()) t.sessionChanged();
  inspector.stateResult = null;
  renderAll();
}

/** 当前身份：EndUser、业务线与鉴权方式（含令牌）。异步请求返回时身份已变，结果属于之前的 EndUser，丢弃。 */
const identity = () => `${S.auth}|${S.bl}|${S.user}|${tokenOf(S.user)}`;

async function newSession() {
  const who = identity();
  const e = chatEntry();
  try {
    await e.sim.ready(5000);
    const sid = await e.sim.createSession(S.agent);
    if (identity() !== who) return;
    openSession(sid);
    toast(`已新建 Session（${S.agent}）`);
    refreshSessions();
  } catch (err) {
    if (identity() !== who) return;
    toast(`新建失败：${err.message}`, "error");
  }
}

let sessions = [];
async function refreshSessions() {
  const who = identity();
  const r = await http("GET", `/v1/sessions?${idQuery()}&limit=30`).catch(() => ({ status: 0 }));
  if (identity() !== who) return;
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
      { class: "segmented" },
      [
        ["chat", "💬 对话"],
        ["devices", "📱 设备"],
        ["xinli", "🎧 旁听"],
        ["console", "🛠 控制台"],
      ].map(([id, name]) => h("button", { class: S.view === id ? "selected" : "", onclick: () => setView(id) }, name)),
    ),
    h(
      "label",
      { class: "top-select" },
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
      { class: "top-select", title: "新建会话时使用" },
      "Agent",
      h(
        "select",
        {
          onchange: (e) => {
            S.agent = e.target.value;
            persist();
            scheduleSide();
          },
        },
        AGENTS.map(([id, desc]) => h("option", { value: id, selected: id === S.agent, title: desc }, id)),
        AGENTS.some(([id]) => id === S.agent) ? null : h("option", { value: S.agent, selected: true }, S.agent),
      ),
    ),
    h("span", { class: "spacer" }),
    h(
      "span",
      { class: `online-chip ${online ? "" : "bad"}`, title: "当前用户在线的设备" },
      h("span", { class: `dot ${online ? "online" : ""}` }),
      `${online}/${live.length} 台设备在线`,
    ),
    h("button", { class: "icon-btn", title: "设置", onclick: () => $("settings").showModal() }, "⚙"),
  );
}

function addUser() {
  const name = prompt("EndUser 名称（-auth jwt 时用 yanshi token -key demo.key -user <名称> 签发令牌）");
  if (!name) return renderTop();
  if (!S.users.some((u) => u.name === name)) S.users.push({ name, token: "" });
  switchUser(name);
}

function switchUser(name) {
  if (S.sid) releaseSession(S.sid);
  S.user = name;
  S.sid = "";
  persist();
  chat?.dispose();
  chat = null;
  selection = null;
  refreshSessions();
  renderAll();
}

// 旁听视图：业务线这一侧的数据（xinli.js）。打开会话时切到"对话"视图。
const xinli = new XinliView({
  http: (method, path) => http(method, path),
  idQuery: () => idQuery(),
  identity: () => identity(),
  openSession: (sid) => {
    openSession(sid);
    refreshSessions();
    setView("chat");
  },
});

function setView(v) {
  S.view = v;
  persist();
  renderAll();
}

// —— 对话视图：会话列表 · 对话 · 详情抽屉 ——

let chat = null;
let selection = null;
let sideQueued = false;
function scheduleSide() {
  if (sideQueued) return;
  sideQueued = true;
  setTimeout(() => {
    sideQueued = false;
    if (S.view === "chat") {
      renderSide();
      renderDrawer();
    }
  }, 200);
}

const chatCtx = () => ({
  artifacts,
  user: S.user,
  children: childView,
  agentName: () => sessions.find((s) => s.session_id === S.sid)?.agent ?? S.agent,
  // 旁听面板的开合在会话之间保持：换到旁听会话后接着提交。
  captureOpen: () => !!S.captureOpen,
  setCaptureOpen: (open) => ((S.captureOpen = open), persist()),
  openSession: (sid) => (openSession(sid), refreshSessions()),
  selected: () => selection,
  onSelect: (sel) => {
    selection = sel;
    S.drawer = true;
    persist();
    layoutChat();
    renderDrawer();
    chat?.schedule();
  },
  onTimeline: (runId) => {
    S.view = "console";
    persist();
    renderAll();
    inspector.focusRun(runId);
  },
  toggleDrawer: () => {
    S.drawer = !S.drawer;
    persist();
    layoutChat();
    renderDrawer();
  },
});

function layoutChat() {
  $("view-chat").classList.toggle("drawer-open", !!S.drawer);
  $("drawer").hidden = !S.drawer;
}

function renderChat() {
  layoutChat();
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
          "在左上角新建会话（⌘K），Agent 在顶栏选择。推荐 ",
          h("b", {}, "concierge"),
          "：它把闹钟、导航、查资料分别委托给专员，在模拟设备上执行，再用卡片汇总结果。",
        ),
        h(
          "div",
          { class: "row center" },
          h("button", { class: "btn primary", onclick: newSession }, `新建会话（${S.agent}）`),
          h("button", { class: "btn", onclick: () => setupScene() }, "一键搭建设备"),
        ),
        h(
          "p",
          { class: "muted" },
          '没有模型密钥时选 console（脚本化模型），输入 call iphone__set_alarm {"time":"07:00"} 这样的命令；加一行 think 20 演示思考过程。',
        ),
      ),
    );
  } else if (!chat || chat.view !== view) {
    chat?.dispose();
    selection = null;
    chat = new ChatPanel({ device: e.sim, view, ctx: chatCtx() });
    fill($("chat-main"), chat.root);
  }
  reportForeground();
  renderSide();
  renderDrawer();
}

function renderDrawer() {
  if (!S.drawer) return;
  const e = devices.get(S.chatDevice[S.user]);
  const view = e && S.sid ? e.views.get(S.sid) : undefined;
  fill(
    $("drawer"),
    drawerContent(selection, {
      view,
      childView,
      openSession,
      onSelect: (sel) => chatCtx().onSelect(sel),
      onTimeline: (runId) => chatCtx().onTimeline(runId),
      close: () => {
        selection = null;
        S.drawer = false;
        persist();
        layoutChat();
        chat?.schedule();
      },
    }),
  );
}

const EXAMPLES = {
  concierge: [
    "明早 7 点叫我起床，顺便导航去公司（望京 SOHO）",
    "我到家前把车里空调开到 22 度，再放点轻音乐",
    "帮我看看电脑上 budget.csv 里哪个月结余最多",
    "这周六下午 3 点和老王打球，帮我记到日历里",
  ],
  assistant: ["明早 7 点叫我起床", "帮我拍张照片看看桌上有什么", "看一下电脑上的 notes.txt", "记住：我对花生过敏"],
  "xinli-day": ["整理今天"],
  "xinli-assistant": ["我最近在忙哪些项目？", "装修那件事现在进展到哪了？"],
  "console-capture": ["call captures {}"],
  console: [
    'think 20\ncall iphone__set_alarm {"time":"07:00","label":"晨跑"}',
    'call show_card {"title":"天气","image":"icon:weather","body":[{"type":"facts","facts":[{"label":"今天","value":"晴 25℃"}]}],"buttons":[{"id":"more","label":"看明天"}]}',
    "ask 选哪个？ | 甲 | 乙",
    'call agent__console-helper {"task":"复述这句话"}',
    "stream 100",
  ],
};

/** 会话的标题：第一条输入的文字（读一次事件，缓存在本机）。 */
const titles = new Map(Object.entries(JSON.parse(localStorage.getItem("yanshi.console.titles") ?? "{}")));
const titleLoading = new Set();
const captureTitles = new Map();
function sessionTitle(s) {
  const t = titles.get(s.session_id);
  if (t !== undefined) return t;
  const c = captureTitles.get(s.session_id);
  if (!titleLoading.has(s.session_id)) {
    titleLoading.add(s.session_id);
    http("GET", `/v1/sessions/${s.session_id}/events?limit=20`).then((r) => {
      // 委托派生的子会话（SessionCreated.parent）不在列表中显示：从父会话的调用详情跳过去。
      if ((r.json?.events ?? [])[0]?.session_created?.parent) {
        children.add(s.session_id);
        localStorage.setItem("yanshi.console.children", JSON.stringify([...children].slice(-300)));
        scheduleSide();
      }
      const ev = (r.json?.events ?? []).find((e) => e.run_requested);
      // 旁听的 Session 往往先有转写、后有输入：还没有输入时以第一段转写为标题，不缓存（之后的输入取代它）。
      const cap = (r.json?.events ?? []).find((e) => e.capture_appended)?.capture_appended.segments?.[0]?.text;
      if (!ev && cap) {
        captureTitles.set(s.session_id, `🎧 ${cap}`);
        scheduleSide();
      }
      if (!ev) {
        // 还没有输入（或读取失败）：稍后再读，不缓存。
        setTimeout(() => titleLoading.delete(s.session_id), 5000);
        return;
      }
      const text = (ev?.run_requested.input ?? [])
        .map((b) => b.text?.text ?? (b.media ? `[${b.media.name}]` : ""))
        .join(" ")
        .trim();
      titles.set(s.session_id, text || "（非文字输入）");
      localStorage.setItem("yanshi.console.titles", JSON.stringify(Object.fromEntries([...titles].slice(-300))));
      scheduleSide();
    });
  }
  return c ?? "";
}

/** 委托派生的子会话（不在列表中显示，当前打开的除外）。 */
const children = new Set(JSON.parse(localStorage.getItem("yanshi.console.children") ?? "[]"));

let sessionFilter = "";
function renderSide() {
  const e = devices.get(S.chatDevice[S.user]);
  const view = e && S.sid ? e.views.get(S.sid) : undefined;
  // 当前会话的标题随第一条输入出现。
  if (view && !titles.get(S.sid)) {
    const first = view.payloads("run_requested")[0];
    if (first) titles.set(S.sid, textOf(first.input) || "（非文字输入）");
    else titles.delete(S.sid);
  }
  const q = sessionFilter.toLowerCase();
  const list = sessions.filter(
    (s) => (!children.has(s.session_id) || s.session_id === S.sid) && (!q || `${sessionTitle(s)} ${s.agent} ${s.session_id}`.toLowerCase().includes(q)),
  );
  const today = new Date().toDateString();
  const groups = [
    ["今天", list.filter((s) => new Date(s.last_input_at || s.created_at).toDateString() === today)],
    ["更早", list.filter((s) => new Date(s.last_input_at || s.created_at).toDateString() !== today)],
  ];
  const agent = S.sid ? (sessions.find((s) => s.session_id === S.sid)?.agent ?? S.agent) : S.agent;
  const search = h("input", { placeholder: "搜索会话…", value: sessionFilter });
  search.addEventListener("input", () => {
    sessionFilter = search.value;
    renderSide();
    $("chat-side").querySelector(".search input")?.focus();
  });
  fill(
    $("chat-side"),
    h("button", { class: "new-session", onclick: newSession }, h("span", {}, "＋ 新建会话"), h("kbd", {}, "⌘K")),
    h("label", { class: "search" }, "🔍", search),
    h(
      "div",
      { class: "sessions" },
      groups.map(([name, xs]) =>
        xs.length
          ? [
              h("div", { class: "list-label" }, name),
              xs.map((s) =>
                h(
                  "div",
                  {
                    class: `session ${s.session_id === S.sid ? "active" : ""} ${s.closed_at ? "closed" : ""}`,
                    onclick: () => s.session_id !== S.sid && openSession(s.session_id),
                  },
                  h("span", { class: "session-title" }, sessionTitle(s) || h("span", { class: "muted" }, "新会话")),
                  h(
                    "span",
                    { class: "session-meta" },
                    s.agent,
                    s.closed_at ? h("span", { class: "closed-label" }, "已关闭") : null,
                    h("time", {}, ago(s.last_input_at && !s.last_input_at.startsWith("0001") ? s.last_input_at : s.created_at)),
                    s.closed_at
                      ? null
                      : h(
                          "button",
                          {
                            class: "close-session",
                            title: "关闭会话（不可逆）",
                            onclick: async (ev) => {
                              ev.stopPropagation();
                              if (!confirm("关闭这个会话？关闭后不能再提交输入。")) return;
                              const r = await http("POST", `/v1/sessions/${s.session_id}/close`, {});
                              if (r.status >= 300) toast(`关闭失败：${r.status}`, "error");
                              refreshSessions();
                            },
                          },
                          "✕",
                        ),
                  ),
                ),
              ),
            ]
          : null,
      ),
      list.length ? null : h("div", { class: "muted pad" }, sessions.length ? "没有匹配的会话" : "还没有会话"),
    ),
    h("span", { class: "spacer" }),
    h(
      "details",
      { class: "examples" },
      h("summary", {}, `试试（${agent}）`),
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
    h(
      "div",
      { class: "device-picker" },
      h("label", {}, "对话所用设备（输入注明来源设备）"),
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
    ),
  );
}

document.addEventListener("keydown", (e) => {
  if (document.querySelector("dialog[open]")) return;
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
    e.preventDefault();
    newSession();
  } else if (e.key === "Escape" && S.view === "chat" && chat) {
    chat.interrupt();
  }
});

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
  const shown = grid.filter((_, i) => !S.onlyOnline || list[i].sim.wanted);
  const setAll = (collapsed) => {
    for (const d of list) d.rec.collapsed = collapsed;
    for (const t of tiles.values()) (t.root.classList.toggle("collapsed", collapsed), t.renderHead());
    persist();
  };
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
            class: "btn small",
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
      h(
        "label",
        { class: "inline" },
        h("input", {
          type: "checkbox",
          checked: !!S.onlyOnline,
          onchange: (e) => {
            S.onlyOnline = e.target.checked;
            persist();
            renderDevices();
          },
        }),
        " 只看在线",
      ),
      h("button", { class: "btn small", onclick: () => setAll(true) }, "全部折叠"),
      h("button", { class: "btn small", onclick: () => setAll(false) }, "全部展开"),
      h("button", { class: "btn small primary", onclick: () => setupScene() }, "一键搭建全套"),
    ),
    h(
      "p",
      { class: "muted hint" },
      "下线：Run 挂起等待设备上线；断网：自动重连、续订、重发未确认的结果；崩溃：执行中的调用留下记录，重新投递后按 call_id 去重。",
    ),
    h(
      "div",
      { class: "grid" },
      shown.length ? shown : h("div", { class: "empty big" }, grid.length ? "没有在线的设备" : "还没有设备。添加一台，或一键搭建全套。"),
    ),
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
  goChat: () => setView("chat"),
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
  return {
    wsUrl: WS_URL,
    httpBase: HTTP,
    businessLine: S.auth === "none" ? S.bl : "",
    agent: "console",
    helper: "console-helper",
    capture: "console-capture",
    users,
  };
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
          h("option", { value: "none", selected: S.auth === "none" }, "自报身份（serve -auth none；局域网调试另加 -insecure-remote）"),
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
  for (const v of ["chat", "devices", "xinli", "console"]) $(`view-${v}`).hidden = S.view !== v;
  if (S.view === "xinli") {
    fill($("xinli-main"), xinli.root);
    xinli.start();
  } else xinli.stop();
  if (S.view === "chat") renderChat();
  if (S.view === "devices") renderDevices();
  if (S.view === "console") {
    inspector.render();
    inspector.poll();
  }
  reportForeground();
}

document.addEventListener("visibilitychange", reportForeground);

// —— 启动 ——

/**
 * detectAuth 探测服务端的鉴权方式：不带令牌请求 Session 列表，-auth none 时返回 200，-auth jwt 时返回 401。
 * 本机保存的设置与服务端不一致（如服务端从 jwt 改为 none 后重启）时以服务端为准，否则设备的 Hello 全部被拒绝。
 */
async function detectAuth() {
  try {
    const r = await fetch(`${HTTP}/v1/sessions?business_line=${encodeURIComponent(S.bl)}&end_user=_probe&limit=1`);
    const mode = r.status === 200 ? "none" : r.status === 401 ? "jwt" : "";
    if (mode && mode !== S.auth) {
      S.auth = mode;
      // 不用 persist：它从已添加的设备重建列表，此时设备还没有添加，会清掉保存的设备。
      localStorage.setItem(SAVED, JSON.stringify(S));
      toast(
        mode === "none" ? "服务端不校验令牌（-auth none），已切换为自报身份" : "服务端要求令牌（-auth jwt）：在 ⚙ 设置中填写各用户的令牌",
        mode === "jwt" ? "error" : "",
      );
    }
  } catch {
    // 服务端不可达：沿用本机设置，设备连接时会重试。
  }
}

await detectAuth();
for (const rec of S.devices) addDevice(rec);
renderSettings();
if (S.sid) for (const d of userDevices()) ensureSubscribed(d);
renderAll();
refreshSessions();
