// "设备"视图中的一台模拟设备：状态与上下线、断网、崩溃；迷你对话；模拟应用（闹钟、日程、短信、照片、音乐、导航、
// 空调、文件）的当前状态；每个能力的执行方式；按 call_id 记账的调用（含手工填写结果）。

import { ChatPanel } from "./chat.js";
import { DEVICE_PRESETS, MODES } from "./device.js";
import { clock, h, hue, short, toast, fill } from "./ui.js";

const MODE_NAMES = { auto: "自动执行", manual: "手工填写结果", error: "返回错误", hang: "不回应", crash: "执行中崩溃" };
const STATE_NAMES = {
  new: "新",
  executing: "执行中",
  awaiting: "待手工填写",
  started: "崩溃前在执行",
  done: "已完成",
  cancelled: "已撤回",
};

export class DeviceTile {
  /**
   * entry：{rec, sim}（app.js 中的设备记录与 SimDevice）。ctx：{artifacts, user, view(entry), children, onRemove(entry), onFocus(entry)}。
   */
  constructor(entry, ctx) {
    this.entry = entry;
    this.ctx = ctx;
    this.tab = entry.rec.tab ?? (DEVICE_PRESETS[entry.rec.preset]?.clientOnly ? "chat" : "apps");
    const p = DEVICE_PRESETS[entry.rec.preset] ?? DEVICE_PRESETS.web;
    this.root = h("div", { class: `tile kind-${entry.rec.preset}`, style: { "--dev": hue(entry.rec.id) } });
    this.head = h("div", { class: "tile-head" });
    this.tabs = h("div", { class: "tile-tabs" });
    this.body = h("div", { class: "tile-body" });
    this.root.append(this.head, this.tabs, this.body);
    this.icon = p.icon;
    this.off = entry.sim.on((n) => {
      if (n.type === "status") this.renderHead();
      if ((n.type === "apps" && this.tab === "apps") || (n.type === "ledger" && this.tab === "calls")) this.renderBody();
      if (n.type === "ledger" && n.entry.state === "awaiting") this.renderTabs();
    });
    this.renderHead();
    this.renderTabs();
    this.renderBody();
  }

  dispose() {
    this.off();
    this.chat?.dispose();
  }

  /** 当前 Session 变化时重建迷你对话。 */
  sessionChanged() {
    this.chat?.dispose();
    this.chat = null;
    if (this.tab === "chat") this.renderBody();
  }

  renderHead() {
    const d = this.entry.sim;
    const rec = this.entry.rec;
    const st = d.status;
    fill(
      this.head,
      h("span", { class: "tile-icon" }, this.icon),
      h(
        "div",
        { class: "tile-name" },
        h("b", {}, d.label || rec.label),
        h("small", {}, `${rec.user} · ${DEVICE_PRESETS[rec.preset]?.title ?? rec.preset}${d.cfg.clientOnly ? " · 只作会话客户端" : ""}`),
      ),
      h("span", { class: `dot ${st}`, title: d.lastError || st }),
      h("span", { class: "spacer" }),
      h(
        "button",
        {
          class: `tiny ${d.wanted ? "" : "primary"}`,
          title: d.wanted ? "下线：断开且不再重连（Run 挂起等待设备上线）" : "上线",
          onclick: () => (d.wanted ? d.disconnect() : d.connect(), this.renderHead(), this.ctx.onChange?.()),
        },
        d.wanted ? "下线" : "上线",
      ),
      h(
        "button",
        { class: "tiny", title: "断网：断开后自动重连（续订、重发未确认的结果）", disabled: !d.online, onclick: () => d.drop() },
        "断网",
      ),
      d.cfg.clientOnly
        ? null
        : h(
            "button",
            {
              class: "tiny",
              title: '进程崩溃：执行中的调用留下"执行中"记录，随后自动重连',
              disabled: !d.online,
              onclick: () => d.drop({ crash: true }),
            },
            "崩溃",
          ),
      h("button", { class: "tiny ghost", title: "移除设备", onclick: () => this.ctx.onRemove(this.entry) }, "✕"),
    );
    this.root.classList.toggle("offline", st !== "online");
  }

  renderTabs() {
    const d = this.entry.sim;
    const awaiting = [...d.ledger.values()].filter((e) => e.state === "awaiting").length;
    const tabs = [["chat", "对话"]];
    if (!d.cfg.clientOnly) tabs.push(["apps", "应用"], ["caps", "能力"], ["calls", awaiting ? `调用 · ${awaiting}` : "调用"]);
    fill(
      this.tabs,
      ...tabs.map(([id, name]) =>
        h(
          "button",
          {
            class: `${this.tab === id ? "active" : ""} ${id === "calls" && awaiting ? "attn" : ""}`,
            onclick: () => ((this.tab = this.entry.rec.tab = id), this.renderTabs(), this.renderBody(), this.ctx.onChange?.()),
          },
          name,
        ),
      ),
    );
  }

  renderBody() {
    switch (this.tab) {
      case "chat":
        return this.renderChat();
      case "apps":
        return fill(this.body, apps(this.entry.sim));
      case "caps":
        return fill(this.body, this.caps());
      case "calls":
        return fill(this.body, this.calls());
    }
  }

  renderChat() {
    const view = this.ctx.view(this.entry);
    if (!view) return fill(this.body, h("div", { class: "empty" }, "没有打开的 Session"));
    if (!this.chat || this.chat.view !== view) {
      this.chat?.dispose();
      this.chat = new ChatPanel({
        device: this.entry.sim,
        view,
        ctx: { artifacts: this.ctx.artifacts, user: this.entry.rec.user, children: this.ctx.children },
        compact: true,
      });
    }
    const focus = h("input", {
      type: "checkbox",
      checked: !!this.entry.sim.subs.get(view.sessionId)?.focused,
      onchange: (e) => this.entry.sim.setActivity(view.sessionId, e.target.checked),
    });
    fill(
      this.body,
      h("label", { class: "inline tile-focus", title: "这台设备在前台显示该 Session（在场，影响提醒与设备歧义消解）" }, focus, " 前台"),
      this.chat.root,
    );
  }

  caps() {
    const d = this.entry.sim;
    return h(
      "table",
      { class: "caps" },
      h("tr", {}, h("th", {}, "能力"), h("th", {}, "执行方式"), h("th", {}, "耗时 ms")),
      d.capabilities.map((c) =>
        h(
          "tr",
          {},
          h(
            "td",
            { title: c.description },
            c.name,
            c.risk === "high" ? h("span", { class: "pill warn", title: "高风险：执行前需审批" }, "审批") : null,
            c.idempotent ? h("span", { class: "pill", title: "幂等：崩溃后可以重新执行" }, "幂等") : null,
          ),
          h(
            "td",
            {},
            h(
              "select",
              { onchange: (e) => ((c.mode = e.target.value), this.ctx.save?.(this.entry)) },
              MODES.map((m) => h("option", { value: m, selected: c.mode === m }, MODE_NAMES[m])),
            ),
          ),
          h(
            "td",
            {},
            h("input", {
              type: "number",
              min: 0,
              step: 100,
              value: c.delayMs ?? 0,
              class: "num",
              onchange: (e) => ((c.delayMs = Number(e.target.value) || 0), this.ctx.save?.(this.entry)),
            }),
          ),
        ),
      ),
    );
  }

  calls() {
    const d = this.entry.sim;
    const list = [...d.ledger.values()].reverse();
    if (!list.length) return h("div", { class: "empty" }, "还没有收到调用");
    return h(
      "div",
      { class: "ledger" },
      list.map((e) => {
        const box = h(
          "div",
          { class: `ledger-entry ${e.state}` },
          h(
            "div",
            { class: "row" },
            h("b", {}, e.capability),
            h("code", { title: e.callId }, short(e.callId)),
            h("span", { class: `pill ${e.state}` }, STATE_NAMES[e.state] ?? e.state),
            h("span", { class: "muted" }, `投递 ${e.deliveries} 次 · 执行 ${e.executions} 次${e.acked ? " · 已确认" : ""}`),
          ),
          h("code", { class: "args" }, e.args),
          e.result ? h("div", { class: e.isError ? "error" : "result" }, e.result) : null,
        );
        if (e.state === "awaiting") {
          const out = h("input", { placeholder: "结果" });
          box.append(
            h(
              "div",
              { class: "row" },
              out,
              h("button", { class: "tiny primary", onclick: () => d.finish(e, out.value || "ok", false) }, "返回结果"),
              h("button", { class: "tiny danger", onclick: () => d.finish(e, out.value || "failed", true) }, "返回错误"),
            ),
          );
        }
        return box;
      }),
    );
  }
}

/** apps 渲染设备上模拟应用的当前状态。 */
function apps(d) {
  const a = d.apps;
  const caps = new Set(d.capabilities.map((c) => c.name));
  const sections = [];
  const section = (icon, title, body, empty = "无") =>
    sections.push(
      h("div", { class: "app" }, h("div", { class: "app-title" }, icon, " ", title), body ?? h("div", { class: "muted" }, empty)),
    );
  if (caps.has("set_alarm")) {
    section(
      "⏰",
      "闹钟",
      a.alarms.length
        ? h(
            "div",
            { class: "alarms" },
            a.alarms.map((x) =>
              h(
                "div",
                { class: "alarm" },
                h("span", { class: "alarm-time" }, x.time),
                h("span", {}, x.label || "闹钟", h("small", {}, " · ", x.repeat)),
                h(
                  "button",
                  {
                    class: "tiny ghost",
                    title: "在设备上删掉（模拟用户手动操作）",
                    onclick: () => ((a.alarms = a.alarms.filter((y) => y !== x)), d.emit({ type: "apps" })),
                  },
                  "✕",
                ),
              ),
            ),
          )
        : null,
      "没有闹钟",
    );
  }
  if (caps.has("start_timer") && a.timers.length) {
    section(
      "⏱️",
      "计时器",
      h(
        "div",
        {},
        a.timers.map((t) => h("div", {}, `${t.label || "计时"} · ${t.minutes} 分钟 · 到 ${clock(t.until)}`)),
      ),
    );
  }
  if (caps.has("create_event")) {
    section(
      "📅",
      "日程",
      a.events.length
        ? h(
            "div",
            {},
            a.events.map((x) =>
              h("div", { class: "event" }, h("b", {}, x.start), " ", x.title, x.place ? h("small", {}, " @", x.place) : null),
            ),
          )
        : null,
      "没有日程",
    );
  }
  if (caps.has("send_sms")) {
    section(
      "✉️",
      "已发短信",
      a.sms.length
        ? h(
            "div",
            {},
            a.sms.map((x) => h("div", { class: "sms" }, h("b", {}, "→ ", x.to), " ", x.text)),
          )
        : null,
      "没有",
    );
  }
  if (caps.has("take_photo"))
    section(
      "🖼️",
      "相册",
      a.photos.length
        ? h(
            "div",
            {},
            a.photos.map((x) => h("div", {}, x.name)),
          )
        : null,
      "没有照片",
    );
  if (caps.has("play_music"))
    section(
      "🎵",
      "音乐",
      h(
        "div",
        {},
        a.music.playing ? `正在播放：${a.music.playing}` : "未在播放",
        caps.has("set_volume") ? ` · 音量 ${a.music.volume}` : "",
      ),
    );
  if (caps.has("navigate"))
    section("🧭", "导航", h("div", {}, a.nav.destination ? `前往 ${a.nav.destination} · 预计 ${a.nav.eta}` : "未在导航"));
  if (caps.has("set_climate")) section("❄️", "空调", h("div", {}, a.climate.on ? `${a.climate.temperature}℃` : "关闭"));
  if (caps.has("get_heart_rate")) section("❤️", "心率", h("div", {}, `${a.heartRate} bpm`));
  if (caps.has("get_location")) section("📍", "位置", h("div", {}, `${a.location.city} · ${a.location.place}`));
  if (caps.has("open_url")) section("🌐", "浏览器", h("div", {}, a.browser.url || "空白页"));
  if (caps.has("read_file")) {
    section(
      "📁",
      "文件",
      h(
        "div",
        { class: "files" },
        Object.entries(d.files).map(([name, body]) =>
          h(
            "details",
            {},
            h("summary", {}, name),
            h("textarea", { rows: 3, value: body, onchange: (e) => ((d.files[name] = e.target.value), toast(`${name} 已修改`)) }),
          ),
        ),
      ),
    );
  }
  return h("div", { class: "apps" }, sections.length ? sections : h("div", { class: "empty" }, "这台设备没有模拟应用"));
}
