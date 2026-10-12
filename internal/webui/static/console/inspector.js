// "控制台"视图：观察交互事件与注入故障。
//   时间线  当前 Session 按 Run、Attempt 分泳道：模型调用、调用（及路由到的 Node）、审批、提问、判断、委托的子 Session
//          （嵌套）、上下文压缩、注入的故障；每个 Run 上方是甘特条。
//   事件    任一设备视角下已提交的事件（JSON）。
//   帧      每台设备收发的每一帧，可手工构造帧发给网关。
//   状态    HTTP API：Session 投影、Node 目录、工件、Memory、配额与用量。
//   故障    服务端故障注入（serve -faults，ADR-0033）。
//   指标    本进程的 yanshi_* 指标（/v1/debug/stats）。
//   验证    验证场景（scenarios.js）。

import { modelLine, splitTool } from "./chat.js";
import { payloadOf, textOf } from "./device.js";
import { SCENARIOS } from "./scenarios.js";
import { clock, fill, h, hue, jsonView, short, toast } from "./ui.js";

const TABS = [
  ["timeline", "时间线"],
  ["events", "事件"],
  ["frames", "帧"],
  ["state", "状态"],
  ["faults", "故障注入"],
  ["metrics", "指标"],
  ["verify", "验证"],
];

const FAULTS = {
  model_error: {
    name: "模型服务端错误（503）",
    hint: "可重试：提供商层先退避重试；次数多于重试上限时 Worker 这一步失败、下一步重来，连续 3 次 Run 失败",
  },
  model_rate_limit: { name: "模型限流（429）", hint: "带 Retry-After，提供商层按它等待后重试", params: { retry_after: 2 } },
  model_stream_cut: {
    name: "流式输出中断",
    hint: "已输出过增量：提供商层不重试，订阅者看到草稿被丢弃，Worker 重新调用",
    params: { after_chars: 20 },
  },
  model_delay: { name: "模型延迟", hint: "便于观察插话、中断与晚加入的设备", params: { delay_ms: 5000 } },
  worker_crash: { name: "Worker 崩溃", hint: "写完这一步后不释放租约：租约过期（serve -lease-ttl）后被接管，开启新的 Attempt" },
  moderation_down: { name: "内容安全不可用", hint: "输入被拒绝（503，不写日志）；模型输出的这一步重试，持续不可用时 Run 失败" },
  catalog_error: { name: "设备目录不可用", hint: "解析工具失败，这一步重试" },
};

const QUICK = [
  ["模型报错一次（重试后成功）", { kind: "model_error", count: 1 }],
  ["模型连续报错 ×3（Run 失败）", { kind: "model_error", count: 3 }],
  ["模型限流一次", { kind: "model_rate_limit", count: 1 }],
  ["回复说到一半断开", { kind: "model_stream_cut", count: 1 }],
  ["模型慢 5 秒 ×2", { kind: "model_delay", count: 2 }],
  ["Worker 崩溃一次", { kind: "worker_crash", count: 1 }],
  ["内容安全挂掉 ×2", { kind: "moderation_down", count: 2 }],
];

export class Inspector {
  /**
   * ctx：
   *   sessionId()、observer() → {device, view}（"对话"所用的设备）、childView(sid)、devices() → [{rec, sim}]、
   *   views() → [{label, view}]（各设备对当前 Session 的视图）、frames（数组，app.js 收集）、onFrames(fn)、
   *   http(method, path, body) → {status, json}（以当前 EndUser 的身份）、verifyEnv()、artifacts、user()
   */
  constructor(root, ctx) {
    this.root = root;
    this.ctx = ctx;
    this.tab = localStorage.getItem("yanshi.console.tab") ?? "timeline";
    this.nav = h("nav", { class: "subtabs" });
    this.body = h("div", { class: "inspect-body" });
    fill(root, this.nav, this.body);
    this.faultState = { rules: [], recent: [], kinds: [], available: null };
    this.metrics = null;
    this.metricsPrev = null;
    this.results = new Map();
    this.frameFilter = { device: "", dir: "", text: "", deltas: false, paused: false };
    this.queued = false;
    ctx.onFrames(() => this.tab === "frames" && this.schedule());
    this.timer = setInterval(() => this.poll(), 2000);
    this.renderNav();
    this.render();
  }

  /** 视图可见时调用：数据变化时重绘当前页签。 */
  schedule() {
    if (this.queued || this.root.closest("[hidden]")) return;
    this.queued = true;
    requestAnimationFrame(() => {
      this.queued = false;
      this.render();
    });
  }

  renderNav() {
    fill(
      this.nav,
      ...TABS.map(([id, name]) =>
        h(
          "button",
          {
            class: this.tab === id ? "active" : "",
            onclick: () => {
              this.tab = id;
              localStorage.setItem("yanshi.console.tab", id);
              this.renderNav();
              this.render();
              this.poll();
            },
          },
          name,
          id === "faults" && this.faultState.rules.length ? h("span", { class: "badge" }, this.faultState.rules.length) : null,
        ),
      ),
    );
  }

  async poll() {
    if (this.root.closest("[hidden]")) return;
    if (this.tab === "faults" || this.tab === "timeline" || this.faultState.available === null) await this.loadFaults();
    if (this.tab === "metrics") await this.loadMetrics();
    if (this.tab === "faults" || this.tab === "metrics") this.render();
  }

  render() {
    switch (this.tab) {
      case "timeline":
        return this.renderTimeline();
      case "events":
        return this.renderEvents();
      case "frames":
        return this.renderFrames();
      case "state":
        return this.renderState();
      case "faults":
        return this.renderFaults();
      case "metrics":
        return this.renderMetrics();
      case "verify":
        return this.renderVerify();
    }
  }

  // —— 时间线 ——

  renderTimeline() {
    const o = this.ctx.observer();
    if (!o?.view) return fill(this.body, h("div", { class: "empty" }, "没有打开的 Session。在顶栏新建或选择一个。"));
    const firings = this.faultState.recent ?? [];
    this.hidden ??= new Set();
    const chip = (cls, name) =>
      h(
        "button",
        {
          class: `lg ${cls} ${this.hidden.has(cls) ? "off" : ""}`,
          title: "点击显示或隐藏这类事件",
          onclick: () => {
            this.hidden.has(cls) ? this.hidden.delete(cls) : this.hidden.add(cls);
            this.render();
          },
        },
        name,
      );
    fill(
      this.body,
      h(
        "div",
        { class: "timeline-wrap" },
        h(
          "div",
          { class: "legend" },
          h("span", { class: "muted" }, "显示："),
          chip("input", "输入"),
          chip("model", "模型调用"),
          chip("call", "调用"),
          chip("approval", "审批 / 提问"),
          chip("delegate", "委托"),
          chip("decision", "判断"),
          chip("fault", "注入的故障"),
          chip("sys", "Attempt / 挂起 / 压缩"),
          this.focus ? h("button", { class: "btn small", onclick: () => ((this.focus = ""), this.render()) }, "展开全部 Run") : null,
          h("span", { class: "muted" }, "时间为相对 Run 开始的偏移，悬停看时刻"),
        ),
        sessionTimeline(o.view, { childView: this.ctx.childView, firings, depth: 0, hidden: this.hidden, focus: this.focus }),
      ),
    );
    if (this.focus) this.body.querySelector(`[data-run="${this.focus}"]`)?.scrollIntoView({ block: "start" });
  }

  /** 定位到某个 Run（"在时间线中查看"）：只展开它并滚动过去。 */
  focusRun(runId) {
    this.tab = "timeline";
    this.focus = runId;
    this.renderNav();
    this.render();
  }

  // —— 事件 ——

  renderEvents() {
    const views = this.ctx.views();
    if (!views.length) return fill(this.body, h("div", { class: "empty" }, "没有打开的 Session"));
    const pick = views.find((v) => v.label === this.eventView) ?? views[0];
    const filter = (this.eventFilter ?? "").toLowerCase();
    const list = pick.view.events.filter((e) => !filter || JSON.stringify(e).toLowerCase().includes(filter));
    const detail = h(
      "div",
      { class: "detail" },
      this.selectedEvent ? jsonView(this.selectedEvent, { open: 4 }) : h("div", { class: "muted" }, "选择一条事件"),
    );
    fill(
      this.body,
      h(
        "div",
        { class: "toolbar" },
        h(
          "label",
          { class: "inline" },
          "视角",
          h(
            "select",
            { onchange: (e) => ((this.eventView = e.target.value), this.render()) },
            views.map((v) => h("option", { value: v.label, selected: v === pick }, v.label)),
          ),
        ),
        h("input", {
          placeholder: "过滤：种类、Run、调用 ID…",
          value: this.eventFilter ?? "",
          oninput: (e) => ((this.eventFilter = e.target.value), this.render()),
        }),
        h("span", { class: "muted" }, `${list.length} / ${pick.view.events.length} 条`),
      ),
      h(
        "div",
        { class: "split" },
        h(
          "div",
          { class: "list" },
          list.map((e) => {
            const [kind, p] = payloadOf(e);
            return h(
              "div",
              { class: `row-item ${this.selectedEvent === e ? "sel" : ""}`, onclick: () => ((this.selectedEvent = e), this.render()) },
              h("code", { class: "seq" }, e.seq),
              h("span", { class: "kind", style: { "--h": hue(kind) } }, kind),
              p.run_id ? h("code", { class: "run", style: { "--h": hue(p.run_id) } }, short(p.run_id)) : null,
              h("span", { class: "summary" }, summarize(kind, p)),
              h("span", { class: "muted time" }, clock(e.time)),
            );
          }),
        ),
        detail,
      ),
    );
  }

  // —— 帧 ——

  renderFrames() {
    const f = this.frameFilter;
    const devices = this.ctx.devices();
    const all = this.ctx.frames;
    const list = all.filter(
      (x) =>
        (!f.device || x.device === f.device) &&
        (!f.dir || x.dir === f.dir) &&
        (f.deltas || x.kind !== "delta") &&
        (!f.text || (x.kind + JSON.stringify(x.msg ?? x.raw)).toLowerCase().includes(f.text.toLowerCase())),
    );
    const shown = list.slice(-600);
    const sel = this.selectedFrame;
    const raw = h(
      "textarea",
      { class: "raw-body", spellcheck: false, placeholder: '{"subscribe": {"session_id": "…"}}' },
      this.rawBody ?? "",
    );
    raw.addEventListener("input", () => (this.rawBody = raw.value));
    // 帧页随新帧重绘：所选的发送目标记在 rawTarget 中，重绘时恢复，否则每来一帧就跳回第一台设备。
    const target = h(
      "select",
      { onchange: (e) => (this.rawTarget = e.target.value) },
      devices.map((d) => h("option", { value: d.rec.id, selected: d.rec.id === this.rawTarget }, d.sim.label || d.rec.label)),
    );
    const sid = this.ctx.sessionId() || "<session>";
    const templates = {
      subscribe: { subscribe: { session_id: sid, after_seq: "0" } },
      submit: {
        request: { request_id: "manual-1", submit: { session_id: sid, input: [{ text: { text: "你好" } }], input_id: "manual-in-1" } },
      },
      "card_action（需要真实卡片）": {
        request: {
          request_id: "manual-2",
          submit: { session_id: sid, input: [{ card_action: { card_id: "…", call_id: "…", action: "…" } }] },
        },
      },
      activity: { activity: { session_id: sid, focused: true, typing: true } },
      "unknown op（应返回错误）": { request: { request_id: "manual-3" } },
    };
    fill(
      this.body,
      h(
        "div",
        { class: "toolbar" },
        h(
          "select",
          { onchange: (e) => ((f.device = e.target.value), this.render()) },
          h("option", { value: "" }, "全部设备"),
          devices.map((d) => h("option", { value: d.rec.id, selected: f.device === d.rec.id }, d.sim.label || d.rec.label)),
        ),
        h(
          "select",
          { onchange: (e) => ((f.dir = e.target.value), this.render()) },
          [
            ["", "双向"],
            ["out", "↑ 设备→网关"],
            ["in", "↓ 网关→设备"],
          ].map(([v, n]) => h("option", { value: v, selected: f.dir === v }, n)),
        ),
        h("input", { placeholder: "过滤：消息种类或内容", value: f.text, oninput: (e) => ((f.text = e.target.value), this.render()) }),
        h(
          "label",
          { class: "inline" },
          h("input", { type: "checkbox", checked: f.deltas, onchange: (e) => ((f.deltas = e.target.checked), this.render()) }),
          " 增量",
        ),
        h("button", { class: "tiny", onclick: () => ((all.length = 0), this.render()) }, "清空"),
        h("span", { class: "muted" }, `${list.length} 帧${list.length > shown.length ? `（显示最近 ${shown.length}）` : ""}`),
      ),
      h(
        "div",
        { class: "split" },
        h(
          "div",
          { class: "list" },
          shown.map((x) =>
            h(
              "div",
              { class: `row-item frame ${x.dir} ${sel === x ? "sel" : ""}`, onclick: () => ((this.selectedFrame = x), this.render()) },
              h("span", { class: "dir" }, x.dir === "out" ? "↑" : "↓"),
              h("span", { class: "dev", style: { "--h": hue(x.device) } }, x.label),
              h("span", { class: "kind" }, x.kind),
              h("span", { class: "summary" }, x.summary),
              h("span", { class: "muted time" }, clock(x.at)),
            ),
          ),
        ),
        h(
          "div",
          { class: "detail-col" },
          h("div", { class: "detail" }, sel ? jsonView(sel.msg ?? sel.raw, { open: 4 }) : h("div", { class: "muted" }, "选择一帧")),
          h(
            "div",
            { class: "raw" },
            h(
              "div",
              { class: "row" },
              h("b", {}, "手工发送"),
              target,
              h(
                "select",
                {
                  onchange: (e) =>
                    e.target.value &&
                    ((raw.value = this.rawBody = JSON.stringify(templates[e.target.value], null, 2)), (e.target.value = "")),
                },
                h("option", { value: "" }, "模板…"),
                Object.keys(templates).map((k) => h("option", { value: k }, k)),
              ),
              h(
                "button",
                {
                  class: "tiny primary",
                  onclick: () => {
                    try {
                      devices.find((d) => d.rec.id === target.value)?.sim.sendRaw(JSON.parse(raw.value));
                    } catch (e) {
                      toast(e.message, "error");
                    }
                  },
                },
                "发送",
              ),
            ),
            raw,
          ),
        ),
      ),
    );
  }

  // —— 状态 ——

  async renderState() {
    const queries = {
      session: ["Session 投影", () => `/v1/sessions/${this.ctx.sessionId()}`],
      nodes: ["Node 目录", () => `/v1/nodes?${this.ctx.idQuery()}`],
      sessions: ["Session 列表", () => `/v1/sessions?${this.ctx.idQuery()}`],
      events: ["事件（HTTP）", () => `/v1/sessions/${this.ctx.sessionId()}/events?limit=500`],
      artifacts: ["工件", () => `/v1/sessions/${this.ctx.sessionId()}/artifacts`],
      memories: ["Memory", () => `/v1/memories?${this.ctx.idQuery()}`],
      quota: ["配额", () => `/v1/quota?${this.ctx.idQuery()}`],
      usage: ["用量", () => `/v1/usage?group_by=model&${this.ctx.idQuery()}`],
    };
    const run = async (q) => {
      this.stateQuery = q;
      const r = await this.ctx.http("GET", queries[q][1]());
      this.stateResult = r;
      this.render();
    };
    if (!this.stateResult && this.ctx.sessionId()) {
      this.stateResult = { status: 0, json: "加载中…" };
      run(this.stateQuery ?? "session");
    }
    const r = this.stateResult;
    fill(
      this.body,
      h(
        "div",
        { class: "toolbar" },
        Object.entries(queries).map(([k, [name]]) =>
          h("button", { class: `tiny ${this.stateQuery === k ? "primary" : ""}`, onclick: () => run(k) }, name),
        ),
        r ? h("span", { class: "muted" }, `HTTP ${r.status}`) : null,
      ),
      h(
        "div",
        { class: "split" },
        h("div", { class: "list state-summary" }, stateSummary(this.stateQuery, r?.json)),
        h("div", { class: "detail" }, r ? jsonView(r.json ?? "", { open: 3 }) : null),
      ),
    );
  }

  // —— 故障注入 ——

  async loadFaults() {
    const r = await this.ctx.http("GET", "/v1/debug/faults").catch(() => ({ status: 0 }));
    const before = this.faultState.rules.length;
    this.faultState =
      r.status === 200 ? { ...r.json, available: true } : { rules: [], recent: [], kinds: [], available: false, status: r.status };
    if (before !== this.faultState.rules.length) this.renderNav();
    if (this.tab === "timeline") this.schedule();
  }

  async addFault(rule) {
    const sid = this.ctx.sessionId();
    const body = { ...rule, session_id: rule.global ? "" : sid };
    delete body.global;
    if (!body.session_id && !rule.global) return toast("先打开一个 Session（或选择对所有 Session 注入）", "error");
    if (rule.params) body.params = Object.fromEntries(Object.entries(rule.params).map(([k, v]) => [k, String(v)]));
    else if (FAULTS[rule.kind]?.params)
      body.params = Object.fromEntries(Object.entries(FAULTS[rule.kind].params).map(([k, v]) => [k, String(v)]));
    const r = await this.ctx.http("POST", "/v1/debug/faults", body);
    if (r.status !== 201) toast(`注入失败：${r.status} ${JSON.stringify(r.json)}`, "error");
    else this.injected = `${FAULTS[rule.kind]?.name ?? rule.kind}${body.count ? ` ×${body.count}` : "（直到删除）"}`;
    await this.loadFaults();
    this.render();
  }

  renderFaults() {
    const st = this.faultState;
    if (st.available === false) {
      return fill(
        this.body,
        h(
          "div",
          { class: "empty big" },
          h("p", {}, "服务端故障注入未开启。"),
          h("p", {}, "以 ", h("code", {}, "./bin/yanshi serve -faults"), " 启动（只用于调试部署，ADR-0033）。"),
          h("p", { class: "muted" }, '设备侧的故障（下线、断网、崩溃、不回应、返回错误）在"设备"视图中，不需要这个开关。'),
        ),
      );
    }
    // 故障页每 2 秒随轮询重绘：编辑过的参数按故障类型保存在 faultParams 中，重绘时沿用，不恢复成默认值。
    const kind = this.faultKind ?? "model_error";
    this.faultParams ??= {};
    this.faultParams[kind] ??= { ...(FAULTS[kind].params ?? {}) };
    const form = {
      kind,
      count: this.faultCount ?? 1,
      global: !!this.faultGlobal,
      params: this.faultParams[kind],
    };
    const paramInputs = Object.entries(form.params).map(([k, v]) =>
      h(
        "label",
        { class: "inline" },
        k,
        h("input", { class: "num", type: "number", value: v, oninput: (e) => (form.params[k] = e.target.value) }),
      ),
    );
    fill(
      this.body,
      h(
        "div",
        { class: "faults" },
        h(
          "div",
          { class: "panel" },
          this.injected
            ? h(
                "div",
                { class: "banner" },
                `已注入：${this.injected}。去对话发一条消息，观察它如何被处理。`,
                h("button", { class: "btn small primary", onclick: () => this.ctx.goChat() }, "去对话 →"),
                h("button", { class: "icon-btn", onclick: () => ((this.injected = ""), this.render()) }, "✕"),
              )
            : null,
          h("h4", {}, "快速注入（当前 Session）"),
          h(
            "div",
            { class: "quick" },
            QUICK.map(([name, rule]) =>
              h("button", { class: "fault-btn", title: FAULTS[rule.kind].hint, onclick: () => this.addFault(rule) }, name),
            ),
          ),
          h("h4", {}, "自定义"),
          h(
            "div",
            { class: "row wrap" },
            h(
              "select",
              { onchange: (e) => ((this.faultKind = e.target.value), this.render()) },
              Object.entries(FAULTS).map(([k, f]) => h("option", { value: k, selected: k === form.kind }, f.name)),
            ),
            h(
              "label",
              { class: "inline" },
              "次数",
              h("input", {
                class: "num",
                type: "number",
                min: 0,
                value: form.count,
                oninput: (e) => (this.faultCount = form.count = Number(e.target.value)),
              }),
            ),
            paramInputs,
            h(
              "label",
              { class: "inline", title: "不限 Session：只有服务令牌或 -auth none 可以添加" },
              h("input", { type: "checkbox", checked: form.global, onchange: (e) => (this.faultGlobal = form.global = e.target.checked) }),
              " 对所有 Session",
            ),
            h("button", { class: "primary", onclick: () => this.addFault(form) }, "注入"),
          ),
          h("p", { class: "muted" }, FAULTS[form.kind].hint, "。次数为 0 表示一直注入，直到删除。"),
        ),
        h(
          "div",
          { class: "panel" },
          h(
            "h4",
            {},
            "生效中的规则 ",
            st.rules.length
              ? h(
                  "button",
                  {
                    class: "tiny",
                    onclick: async () => (await this.ctx.http("DELETE", "/v1/debug/faults"), await this.loadFaults(), this.render()),
                  },
                  "全部删除",
                )
              : null,
          ),
          st.rules.length
            ? h(
                "table",
                { class: "tbl" },
                h(
                  "tr",
                  {},
                  h("th", {}, "规则"),
                  h("th", {}, "类型"),
                  h("th", {}, "Session"),
                  h("th", {}, "已注入 / 次数"),
                  h("th", {}, ""),
                ),
                st.rules.map((r) =>
                  h(
                    "tr",
                    {},
                    h("td", {}, h("code", {}, r.id)),
                    h("td", {}, FAULTS[r.kind]?.name ?? r.kind),
                    h("td", {}, r.session_id ? h("code", {}, short(r.session_id)) : "全部"),
                    h("td", {}, `${r.fired} / ${r.count || "∞"}`),
                    h(
                      "td",
                      {},
                      h(
                        "button",
                        {
                          class: "tiny",
                          onclick: async () => (
                            await this.ctx.http("DELETE", `/v1/debug/faults?id=${r.id}`),
                            await this.loadFaults(),
                            this.render()
                          ),
                        },
                        "删除",
                      ),
                    ),
                  ),
                ),
              )
            : h("div", { class: "muted" }, "没有"),
          h("h4", {}, "最近的注入"),
          st.recent?.length
            ? h(
                "div",
                { class: "list compact" },
                st.recent
                  .slice(0, 50)
                  .map((f) =>
                    h(
                      "div",
                      { class: "row-item" },
                      h("span", { class: "muted time" }, clock(f.time)),
                      h("span", { class: "kind fault" }, FAULTS[f.kind]?.name ?? f.kind),
                      f.session_id ? h("code", {}, short(f.session_id)) : null,
                      h("code", { class: "muted" }, f.rule_id),
                    ),
                  ),
              )
            : h("div", { class: "muted" }, "还没有"),
        ),
      ),
    );
  }

  // —— 指标 ——

  async loadMetrics() {
    const r = await this.ctx.http("GET", "/v1/debug/stats").catch(() => ({ status: 0 }));
    this.metricsPrev = this.metrics;
    this.metrics = r.status === 200 ? { at: Date.now(), data: r.json } : { error: r.status };
  }

  renderMetrics() {
    const m = this.metrics;
    if (!m) return fill(this.body, h("div", { class: "empty" }, "加载中…"));
    if (m.error !== undefined)
      return fill(
        this.body,
        h(
          "div",
          { class: "empty big" },
          h("p", {}, "指标接口未开启：以 ", h("code", {}, "serve -faults"), " 启动后可用（/v1/debug/stats）。"),
        ),
      );
    const d = m.data;
    const sum = (name, pred = () => true) =>
      (d[name] ?? []).filter((x) => pred(x.labels ?? {})).reduce((s, x) => s + (x.value || x.count || 0), 0);
    const prev = this.metricsPrev?.data;
    const rate = (name, pred) => {
      if (!prev) return "";
      const before = (prev[name] ?? []).filter((x) => pred?.(x.labels ?? {}) ?? true).reduce((s, x) => s + (x.value || x.count || 0), 0);
      const dt = (m.at - this.metricsPrev.at) / 1000;
      const r = (sum(name, pred) - before) / dt;
      return r > 0 ? `+${r.toFixed(1)}/s` : "";
    };
    // 计数器的趋势画每次刷新的增量，当前值（gauge）画值本身；保留最近 40 次刷新。
    const tiles = [
      ["Run 完成", sum("yanshi_runs_finished_total", (l) => l.status === "completed"), rate("yanshi_runs_finished_total"), true],
      ["Run 失败", sum("yanshi_runs_finished_total", (l) => l.status === "failed"), "", true],
      ["模型调用", sum("yanshi_model_call_duration_seconds"), rate("yanshi_model_call_duration_seconds"), true],
      ["模型重试", sum("yanshi_model_retries_total"), rate("yanshi_model_retries_total"), true],
      ["Attempt（含接管）", sum("yanshi_attempts_total"), rate("yanshi_attempts_total"), true],
      ["注入的故障", sum("yanshi_faults_injected_total"), rate("yanshi_faults_injected_total"), true],
      ["队列中可认领", sum("yanshi_queue_ready"), "", false],
      ["在线连接", sum("yanshi_nodes_connected"), "", false],
    ];
    this.hist ??= new Map();
    if (this.histAt !== m.at) {
      this.histAt = m.at;
      for (const [name, v] of tiles) this.hist.set(name, [...(this.hist.get(name) ?? []), v].slice(-41));
    }
    const filter = (this.metricFilter ?? "").toLowerCase();
    const groups = new Map(GROUPS.map(([name]) => [name, []]));
    for (const [name, series] of Object.entries(d).sort()) {
      if (filter && !name.includes(filter)) continue;
      const rows = groups.get(GROUPS.find(([, re]) => re.test(name))?.[0] ?? "其他");
      for (const s of series) {
        const labels = Object.entries(s.labels ?? {})
          .map(([k, v]) => `${k}=${v}`)
          .join(" ");
        rows.push(
          h(
            "tr",
            {},
            h("td", {}, h("code", {}, name.replace(/^yanshi_/, ""))),
            h("td", { class: "muted" }, labels),
            h(
              "td",
              { class: "num" },
              s.count !== undefined && s.count ? `${s.count} 次 · 平均 ${(s.sum / s.count).toFixed(3)}s` : fmt(s.value),
            ),
          ),
        );
      }
    }
    fill(
      this.body,
      h(
        "div",
        { class: "tiles" },
        tiles.map(([name, v, r, counter]) =>
          h(
            "div",
            { class: "metric" },
            h("div", { class: "metric-n" }, name),
            h("div", { class: "metric-v" }, fmt(v), r ? h("span", { class: "metric-r" }, r) : null),
            spark(this.hist.get(name) ?? [], counter),
          ),
        ),
      ),
      h(
        "div",
        { class: "toolbar" },
        h("input", {
          placeholder: "过滤指标名…",
          value: this.metricFilter ?? "",
          oninput: (e) => ((this.metricFilter = e.target.value), this.render()),
        }),
        h("span", { class: "muted" }, "每 2 秒刷新；只含 ID 与计数，不含内容"),
      ),
      h(
        "div",
        { class: "metric-groups" },
        [...groups]
          .filter(([, rows]) => rows.length)
          .map(([name, rows]) => h("section", { class: "panel" }, h("h4", {}, name), h("table", { class: "tbl metrics" }, rows))),
      ),
    );
  }

  // —— 验证 ——

  renderVerify() {
    const env = this.ctx.verifyEnv();
    const run = async (list) => {
      for (const s of list) {
        this.results.set(s.id, { running: true, log: [] });
        this.render();
        const r = await this.ctx.runScenario(s, env, (m) => {
          this.results.get(s.id).log.push(m);
          this.schedule();
        });
        this.results.set(s.id, { ...r, log: this.results.get(s.id).log });
        this.render();
      }
    };
    const done = [...this.results.values()].filter((r) => !r.running);
    fill(
      this.body,
      h(
        "div",
        { class: "toolbar" },
        h("button", { class: "primary tiny", onclick: () => run(SCENARIOS) }, "运行全部"),
        h("span", { class: "muted" }, done.length ? `${done.filter((r) => r.ok).length} / ${done.length} 通过` : ""),
        h("span", { class: "muted" }, `每个场景用自己的模拟设备与 Session 对网关逐项检查；Agent：${env.agent}（须为脚本化模型）`),
      ),
      h(
        "div",
        { class: "list" },
        SCENARIOS.map((s) => {
          const r = this.results.get(s.id);
          const status = !r ? "" : r.running ? "运行中…" : r.skipped ? "跳过" : r.ok ? `通过 · ${r.ms} ms` : "失败";
          return h(
            "details",
            {
              class: `scenario ${r ? (r.running ? "running" : r.skipped ? "skipped" : r.ok ? "ok" : "fail") : ""}`,
              open: r && !r.running && !r.ok && !r.skipped,
            },
            h(
              "summary",
              {},
              h("b", {}, s.title),
              h("span", { class: "muted" }, " ", s.description ?? ""),
              h("span", { class: "spacer" }),
              h("span", { class: "status" }, status),
              h("button", { class: "tiny", onclick: (e) => (e.preventDefault(), run([s])) }, "运行"),
            ),
            r ? h("pre", { class: "scenario-log" }, [...r.log, r.error ? `✗ ${r.error}` : ""].join("\n")) : null,
          );
        }),
      ),
    );
  }
}

// 指标分组：按名称前缀。
const GROUPS = [
  ["Run 与 Attempt", /^yanshi_(runs?_|attempts_|handoffs_|compactions_|commit_conflicts_)/],
  ["模型调用", /^yanshi_model_/],
  ["队列与准入", /^yanshi_(queue_|admission_)/],
  ["判断", /^yanshi_decision/],
  ["内容安全", /^yanshi_moderation_/],
  ["连接与通话", /^yanshi_(http_|nodes_|handshakes_|live_|calls?_)/],
  ["计量与配额", /^yanshi_(usage_|quota_)/],
  ["存储", /^yanshi_session_/],
  ["故障注入", /^yanshi_faults_/],
  ["其他", /./],
];

/** spark 是趋势小图：计数器画相邻两次刷新的差，当前值画值本身。 */
function spark(values, counter) {
  const ys = counter ? values.slice(1).map((v, i) => v - values[i]) : values;
  if (ys.length < 2) return h("div", { class: "spark empty" });
  const max = Math.max(...ys, 1);
  const pts = ys.map((y, i) => `${((i / (ys.length - 1)) * 100).toFixed(1)},${(28 - (y / max) * 26).toFixed(1)}`).join(" ");
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("viewBox", "0 0 100 30");
  svg.setAttribute("preserveAspectRatio", "none");
  svg.setAttribute("class", "spark");
  const line = document.createElementNS("http://www.w3.org/2000/svg", "polyline");
  line.setAttribute("points", pts);
  svg.append(line);
  return svg;
}

/** 相对 Run 开始的偏移："+1.2s"。 */
function offset(ms) {
  if (!Number.isFinite(ms)) return "";
  return `+${(Math.max(0, ms) / 1000).toFixed(1)}s`;
}

function fmt(v) {
  if (typeof v !== "number") return String(v ?? "");
  return Number.isInteger(v) ? String(v) : v.toFixed(3);
}

/** sessionTimeline 渲染一个 Session 的全部 Run；委托的子 Session 嵌套在委托调用下面。 */
function sessionTimeline(view, opts) {
  const runs = view.payloads("run_requested");
  if (!runs.length) return h("div", { class: "muted" }, "还没有 Run");
  return h(
    "div",
    { class: `session-tl depth-${opts.depth}` },
    runs.map((r) => runTimeline(view, r, opts)),
  );
}

function runTimeline(view, req, opts) {
  const runId = req.run_id;
  const events = view.events.filter((e) => {
    const [, p] = payloadOf(e);
    return p.run_id === runId;
  });
  const t0 = Date.parse(events[0]?.time ?? 0);
  const end = view.runEnd(runId);
  const last = events.find((e) => ["run_completed", "run_failed", "run_interrupted"].includes(payloadOf(e)[0]));
  const tEnd = last ? Date.parse(last.time) : Date.now();
  const span = Math.max(1, tEnd - t0);
  const attempts = view.payloads("attempt_started").filter((a) => a.run_id === runId);
  const status = end ? { run_completed: "完成", run_failed: "失败", run_interrupted: "中断" }[end.kind] : "进行中";
  // 甘特条：每个 Attempt 一段，调用一段。
  const pos = (t) => `${Math.max(0, Math.min(100, ((Date.parse(t) - t0) / span) * 100))}%`;
  const attemptBars = attempts.map((a, i) => {
    const start = view.events.find((e) => e.attempt_started?.run_id === runId && e.attempt_started?.attempt === a.attempt);
    const next =
      attempts[i + 1] &&
      view.events.find((e) => e.attempt_started?.run_id === runId && e.attempt_started?.attempt === attempts[i + 1].attempt);
    const left = pos(start?.time);
    const right = next ? pos(next.time) : "100%";
    return h(
      "div",
      { class: "bar attempt", style: { left, width: `calc(${right} - ${left})`, "--h": hue(`${runId}/${a.attempt}`) } },
      `Attempt ${a.attempt}`,
    );
  });
  const callBars = [];
  for (const e of events) {
    if (!e.tool_call_started) continue;
    const res = events.find((x) => x.tool_result?.call_id === e.tool_call_started.call_id);
    const left = pos(e.time);
    const right = res ? pos(res.time) : "100%";
    callBars.push(
      h("div", {
        class: `bar call ${res?.tool_result?.is_error ? "error" : ""}`,
        style: { left, width: `calc(${right} - ${left})` },
        title: view.callCapability(e.tool_call_started.call_id),
      }),
    );
  }
  const firings = (opts.firings ?? []).filter(
    (f) => f.session_id === view.sessionId && Date.parse(f.time) >= t0 - 1000 && Date.parse(f.time) <= tEnd + 1000,
  );
  const items = timelineItems(view, events, { ...opts, t0 });
  for (const f of opts.hidden?.has("fault") ? [] : firings)
    items.push({
      t: Date.parse(f.time),
      node: h(
        "div",
        { class: "tl-item fault" },
        h("span", { class: "tl-time", title: clock(f.time) }, offset(Date.parse(f.time) - t0)),
        h("span", { class: "tl-lane" }),
        h("b", {}, "⚡ 注入故障 "),
        f.kind,
        h("code", { class: "muted" }, ` ${f.rule_id}`),
      ),
    });
  items.sort((a, b) => a.t - b.t);
  return h(
    "details",
    {
      class: `run-tl ${end?.kind ?? "running"}`,
      open:
        opts.focus && opts.depth === 0
          ? opts.focus === runId
          : !end || opts.depth > 0 || view.payloads("run_requested").at(-1)?.run_id === runId,
      style: { "--h": hue(runId) },
      dataset: { run: runId },
    },
    h(
      "summary",
      {},
      h("code", { class: "run" }, short(runId)),
      h("span", { class: "run-input", title: inputText(req.input, Infinity) }, inputText(req.input) || "（无文本）"),
      h("span", { class: `pill ${end?.kind ?? "running"}` }, status),
      h("span", { class: "muted" }, `${attempts.length} 次 Attempt · ${((tEnd - t0) / 1000).toFixed(1)}s`),
    ),
    h(
      "div",
      { class: "gantt" },
      h("div", { class: "lane" }, attemptBars),
      h("div", { class: "lane calls" }, callBars),
      firings.map((f) => h("div", { class: "bar fault-mark", style: { left: pos(f.time) }, title: f.kind })),
    ),
    h(
      "div",
      { class: "tl-items" },
      items.map((x) => x.node),
    ),
  );
}

function timelineItems(view, events, opts) {
  const out = [];
  const lane = (attempt) =>
    h(
      "span",
      {
        class: "tl-lane",
        style: { "--h": hue(`${events[0] ? payloadOf(events[0])[1].run_id : ""}/${attempt}`) },
        title: `Attempt ${attempt}`,
      },
      attempt ? `A${attempt}` : "外部",
    );
  const item = (e, cls, attempt, ...body) => {
    if (opts.hidden?.has(cls.split(" ")[0])) return;
    const text = h("span", { class: "tl-text" }, body);
    text.title = text.textContent;
    out.push({
      t: Date.parse(e.time),
      node: h(
        "div",
        { class: `tl-item ${cls}` },
        h("span", { class: "tl-time", title: clock(e.time) }, offset(Date.parse(e.time) - opts.t0)),
        lane(attempt),
        text,
      ),
    });
  };
  for (const e of events) {
    const [kind, p] = payloadOf(e);
    switch (kind) {
      case "run_requested":
      case "steered":
        item(e, "input", 0, h("b", {}, kind === "steered" ? "插话 " : "输入 "), inputText(p.input));
        break;
      case "attempt_started":
        item(
          e,
          "sys",
          p.attempt,
          h("b", {}, `Attempt ${p.attempt} 开始`),
          p.attempt > 1 ? h("span", { class: "warn" }, " （接管或恢复）") : null,
        );
        break;
      case "assistant_message": {
        const calls = (p.tool_calls ?? []).map((c) =>
          splitTool(c.capability)[1] ? `${splitTool(c.capability)[1]}·${splitTool(c.capability)[0]}` : c.capability,
        );
        item(
          e,
          "model",
          p.attempt,
          h("b", {}, "模型 "),
          h("span", { class: "muted" }, modelLine(p)),
          p.reasoning ? h("span", { class: "muted" }, ` · 💭 ${p.reasoning.length} 字思考`) : null,
          " ",
          calls.length ? `→ ${calls.join("、")}` : textOf(p.content).slice(0, 120),
        );
        break;
      }
      case "tool_call_started": {
        const cap = view.callCapability(p.call_id);
        const node = p.node_id
          ? p.node_id.startsWith("@agent/")
            ? `子 Session ${short(p.node_id.slice(7))}`
            : p.node_id === "@user"
              ? "用户"
              : short(p.node_id)
          : "进程内";
        const child = p.node_id?.startsWith("@agent/") ? opts.childView?.(p.node_id.slice(7)) : undefined;
        item(
          e,
          child || p.node_id?.startsWith("@agent/") ? "delegate" : "call",
          p.attempt,
          h("b", {}, child || p.node_id?.startsWith("@agent/") ? "委托 " : "调用 "),
          h("code", {}, cap),
          h("span", { class: "muted" }, ` → ${node}`),
          child ? h("div", { class: "child" }, sessionTimeline(child, { ...opts, depth: opts.depth + 1 })) : null,
        );
        break;
      }
      case "tool_result":
        item(
          e,
          p.is_error ? "call error" : "call",
          p.attempt,
          h("b", {}, p.is_error ? "结果（错误） " : "结果 "),
          h("code", {}, view.callCapability(p.call_id)),
          " ",
          textOf(p.content).slice(0, 160),
          (p.content ?? []).some((b) => b.card) ? h("span", { class: "pill" }, "卡片") : null,
        );
        break;
      case "approval_requested":
        item(e, "approval", 0, h("b", {}, "请求审批 "), p.summary);
        break;
      case "approval_decided":
        item(e, "approval", 0, h("b", {}, p.approved ? "已批准 " : "已拒绝 "), h("span", { class: "muted" }, p.by ?? ""));
        break;
      case "run_suspended":
        item(e, "sys", p.attempt, h("b", {}, "挂起 "), h("span", { class: "muted" }, p.reason ?? "等待外部结果"));
        break;
      case "context_compacted":
        item(e, "sys", p.attempt, h("b", {}, "上下文压缩"));
        break;
      case "memory_recalled":
        item(e, "sys", p.attempt, h("b", {}, "召回 Memory "), `${(p.items ?? []).length} 条`);
        break;
      case "decision_made":
        item(
          e,
          "decision",
          p.attempt,
          h("b", {}, `判断 ${p.kind} `),
          `${p.chosen || "（不可用，走默认路径）"}`,
          h(
            "span",
            { class: "muted" },
            ` · ${p.model} · 置信度 ${(p.confidence ?? 0).toFixed?.(2) ?? p.confidence}${p.shadow ? " · 影子模式" : ""}`,
          ),
        );
        break;
      case "content_moderated":
        item(e, "fault", p.attempt, h("b", {}, "内容安全拦截 "), p.stage);
        break;
      case "run_completed":
      case "run_failed":
      case "run_interrupted":
        item(
          e,
          kind === "run_completed" ? "end ok" : "end bad",
          p.attempt ?? 0,
          h("b", {}, { run_completed: "完成", run_failed: "失败 ", run_interrupted: "中断" }[kind]),
          p.reason ?? "",
        );
        break;
    }
  }
  return out;
}

function inputText(input, max = 160) {
  const parts = [];
  for (const b of input ?? []) {
    if (b.text?.text) parts.push(b.text.text);
    else if (b.media) parts.push(`[${(b.media.mime_type ?? "").split("/")[0] || "文件"}：${b.media.name}]`);
    else if (b.card_action) parts.push(`[卡片动作：${b.card_action.label || b.card_action.action}]`);
  }
  return parts.join(" ").slice(0, max);
}

/** summarize 是事件的一行摘要（事件列表）。 */
export function summarize(kind, p) {
  switch (kind) {
    case "session_created":
      return `${p.business_line}/${p.end_user} · ${p.agent?.name}@${p.agent?.version}${p.parent ? ` · 父 ${short(p.parent.session_id)}` : ""}`;
    case "run_requested":
    case "steered":
      return inputText(p.input);
    case "assistant_message":
      return (p.tool_calls ?? []).length
        ? `→ ${(p.tool_calls ?? []).map((c) => c.capability).join("、")}`
        : textOf(p.content).slice(0, 100);
    case "tool_call_started":
      return `${p.call_id} → ${p.node_id || "进程内"}`;
    case "tool_result":
      return `${p.call_id}${p.is_error ? " ✗" : ""} ${textOf(p.content).slice(0, 80)}`;
    case "approval_requested":
      return p.summary;
    case "approval_decided":
      return `${p.call_id} ${p.approved ? "批准" : "拒绝"}`;
    case "run_failed":
      return p.reason;
    case "run_suspended":
      return p.reason ?? "";
    case "decision_made":
      return `${p.kind} → ${p.chosen}`;
    case "call_transcript":
      return `${p.role}：${p.text}`;
    default:
      return "";
  }
}

function stateSummary(q, json) {
  if (!json || typeof json !== "object") return h("div", { class: "muted" }, String(json ?? ""));
  const rows = [];
  const kv = (k, v) => rows.push(h("div", { class: "kv" }, h("span", { class: "muted" }, k), h("span", {}, v)));
  switch (q) {
    case "session": {
      kv("Agent", `${json.agent?.name ?? ""}@${json.agent?.version ?? ""}`);
      kv("状态", json.closed ? "已关闭" : "打开");
      for (const r of json.runs ?? [])
        kv(`Run ${short(r.run_id)}`, `${r.status}${r.waiting ? ` · 等待：${JSON.stringify(r.waiting)}` : ""}`);
      if (json.parent) kv("父 Session", short(json.parent.session_id));
      break;
    }
    case "nodes":
      for (const n of json.nodes ?? []) kv(n.label || n.node_id, `${n.online ? "在线" : "离线"} · ${(n.capabilities ?? []).join("、")}`);
      break;
    default:
      kv("返回", Array.isArray(json) ? `${json.length} 项` : `${Object.keys(json).length} 个字段`);
  }
  return rows;
}
