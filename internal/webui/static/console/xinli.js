// 旁听视图（docs/design/console.md §2.1）：旁听业务线 xinli 这一侧的数据，按 App 的页面展示——
//   今日     每次每日整理的总览、要点，以及当天交出的交谈（定下的事、风险、归属）
//   项目     项目的现状、下一步与按交谈的时间线（xinli-project 的 set_status）
//   承诺     承诺账本：双方各自答应的事，按被引用原话的说话人核对（docs/design/captures-by-exchange.md §7）
//   待确认   归属拿不准的交谈：替用户确认归入候选项目或新建
//   觉察     用户本人的沟通习惯与建议（按天），以及旁听写入的长期 Memory
// 数据来自模拟的 xinli MCP Server（serve -xinli-mock，GET /v1/debug/xinli），可跳到对应的 Session。

import { ago, fill, h } from "./ui.js";

const TABS = [
  ["today", "今日"],
  ["projects", "项目"],
  ["commitments", "承诺"],
  ["pending", "待确认"],
  ["awareness", "觉察"],
];

export class XinliView {
  /**
   * @param {object} ctx
   *   http(method, path, body)   调用 HTTP API，返回 {status, json}
   *   idQuery()            -auth none 时的 business_line 与 end_user 参数
   *   openSession(sid)     在"对话"视图中打开 Session
   *   identity()           当前 EndUser 与业务线的标识：变化时丢弃在途的结果
   */
  constructor(ctx) {
    this.ctx = ctx;
    this.tab = "today";
    this.state = null;
    this.memories = [];
    this.error = "";
    this.openProject = "";
    this.root = h("div", { class: "xinli" });
    this.timer = null;
  }

  /** 视图可见时每 3 秒刷新一次：每日整理与归集在后台进行。 */
  start() {
    if (this.timer) return;
    this.refresh();
    this.timer = setInterval(() => document.visibilityState === "visible" && this.refresh(), 3000);
  }

  stop() {
    clearInterval(this.timer);
    this.timer = null;
  }

  async refresh() {
    const who = this.ctx.identity();
    const q = this.ctx.idQuery();
    const [st, mem] = await Promise.all([
      this.ctx.http("GET", `/v1/debug/xinli?${q}`).catch((e) => ({ status: 0, json: String(e) })),
      this.ctx.http("GET", `/v1/memories?${q}&limit=100`).catch(() => ({ status: 0 })),
    ]);
    if (who !== this.ctx.identity()) return;
    if (st.status === 200) {
      this.state = st.json;
      this.error = "";
    } else {
      this.state = null;
      this.error =
        st.status === 404
          ? "服务端没有开启模拟的 xinli 业务线：以 serve -xinli-mock <业务线> 启动（dev serve 默认 -xinli-mock demo）。"
          : `读取失败：${st.status} ${typeof st.json === "string" ? st.json : (st.json?.error ?? "")}`;
    }
    this.memories = mem.status === 200 ? (mem.json?.memories ?? []) : [];
    this.render();
  }

  render() {
    const body = this.error
      ? h("div", { class: "empty" }, this.error)
      : !this.state
        ? h("div", { class: "empty" }, "读取中…")
        : ({ today: () => this.today(), projects: () => this.projects(), commitments: () => this.commitments(), pending: () => this.pending() }[
            this.tab
          ] ?? (() => this.awareness()))();
    fill(
      this.root,
      h(
        "div",
        { class: "xinli-head" },
        h(
          "nav",
          { class: "segmented" },
          TABS.map(([id, name]) =>
            h(
              "button",
              { class: this.tab === id ? "selected" : "", onclick: () => ((this.tab = id), this.render()) },
              id === "pending" && this.pendingList().length ? `${name} ${this.pendingList().length}` : name,
            ),
          ),
        ),
        h(
          "span",
          { class: "muted" },
          "在对话中新建 xinli-day 会话，🎧 提交旁听后发「整理今天」；整理之后业务线为每个项目提交「整理 <日期>」，更新现状、对账承诺",
        ),
      ),
      h("div", { class: "xinli-body" }, body),
    );
  }

  // —— 今日 ——

  today() {
    const days = this.state.days ?? [];
    if (!days.length) return h("div", { class: "empty" }, "还没有每日整理。");
    return days.map((d) =>
      h(
        "section",
        { class: "xinli-card" },
        h(
          "div",
          { class: "xinli-card-head" },
          h("b", {}, d.date),
          h("span", { class: "muted" }, ` · 整理于 ${ago(d.filed_at)}`),
          h("span", { class: "spacer" }),
          this.sessionLink(d.session, "打开每日整理会话"),
        ),
        h("div", { class: "xinli-headline" }, d.headline || "（没有总览）"),
        list("要点", d.highlights),
        this.exchangesOf((e) => e.session === d.session).map((e) => this.exchange(e, true)),
      ),
    );
  }

  exchangesOf(pred) {
    return (this.state.exchanges ?? []).filter(pred);
  }

  projectName(id) {
    return (this.state.projects ?? []).find((p) => p.id === id)?.name ?? "";
  }

  /** 一段交谈；withWhere 时注明归属，可跳到项目。 */
  exchange(e, withWhere) {
    let where = null;
    if (withWhere) {
      where = e.project
        ? h("a", { href: "#", onclick: (ev) => (ev.preventDefault(), this.showProject(e.project)) }, `${e.project} ${this.projectName(e.project)}`)
        : e.match === "unsure"
          ? h("span", { class: "tag" }, `待确认：可能属于 ${e.candidate}`)
          : h("span", { class: "muted" }, "不归项目");
    }
    const cms = (this.state.commitments ?? []).filter((m) => m.session === e.session && m.exchange === e.id);
    return h(
      "div",
      { class: "xinli-group" },
      h("div", { class: "xinli-group-title" }, h("b", {}, e.title), " ", h("code", {}, e.id), where ? [" · ", where] : null),
      h("div", {}, e.summary),
      list("已定", e.decisions, "decided"),
      cms.length ? commitmentList("承诺", cms) : null,
      list("风险", e.risks, "risk"),
    );
  }

  // —— 项目 ——

  showProject(id) {
    this.tab = "projects";
    this.openProject = id;
    this.render();
  }

  projects() {
    const ps = this.state.projects ?? [];
    if (!ps.length) return h("div", { class: "empty" }, "还没有项目。每日整理会为值得跟进的新主题建立项目。");
    return ps.map((p) => {
      const open = this.openProject === p.id;
      const exs = this.exchangesOf((e) => e.project === p.id);
      const cms = (this.state.commitments ?? []).filter((m) => m.project === p.id && m.status === "open");
      return h(
        "section",
        { class: `xinli-card ${open ? "open" : ""}` },
        h(
          "div",
          { class: "xinli-card-head", onclick: () => ((this.openProject = open ? "" : p.id), this.render()) },
          h("b", {}, p.name),
          h("code", {}, p.id),
          h("span", { class: "muted" }, ` · ${exs.length} 段交谈 · ${cms.length} 条未完成的承诺`),
          h("span", { class: "spacer" }),
          p.session ? this.sessionLink(p.session, "打开项目会话") : h("span", { class: "muted" }, "尚未归集"),
        ),
        h("div", {}, p.status || h("span", { class: "muted" }, "还没有现状"), p.status_at ? h("small", { class: "muted" }, ` · ${ago(p.status_at)}更新`) : null),
        list("下一步", p.next_steps),
        list("待解决", p.open_questions, "risk"),
        cms.length ? commitmentList("承诺", cms) : null,
        open
          ? h(
              "div",
              { class: "xinli-timeline" },
              exs
                .slice()
                .reverse()
                .map((e) => h("div", { class: "xinli-entry" }, h("div", { class: "xinli-entry-date" }, e.date), this.exchange(e, false))),
            )
          : null,
      );
    });
  }

  // —— 承诺 ——

  commitments() {
    const cms = this.state.commitments ?? [];
    if (!cms.length) return h("div", { class: "empty" }, "还没有承诺。每日整理交出交谈时，双方各自答应的事会进入账本。");
    const groups = [
      ["我答应的", cms.filter((m) => m.kind === "mine" && m.status === "open")],
      ["别人答应我的", cms.filter((m) => m.kind === "theirs" && m.status === "open")],
      ["已完成或取消", cms.filter((m) => m.status !== "open")],
    ];
    return groups
      .filter(([, list]) => list.length)
      .map(([label, list]) => h("section", { class: "xinli-card" }, h("div", { class: "xinli-card-head" }, h("b", {}, label)), commitmentList("", list, true)));
  }

  // —— 待确认 ——

  pendingList() {
    return this.exchangesOf((e) => e.match === "unsure" && !e.project);
  }

  pending() {
    const list = this.pendingList();
    if (!list.length) return h("div", { class: "empty" }, "没有待确认的交谈。每日整理拿不准一段交谈属于哪个项目时，会放在这里请用户确认。");
    return list.map((e) =>
      h(
        "section",
        { class: "xinli-card" },
        h("div", { class: "xinli-card-head" }, h("b", {}, e.date), h("span", { class: "spacer" }), this.sessionLink(e.session, "打开每日整理会话")),
        this.exchange(e, false),
        h(
          "div",
          { class: "xinli-actions" },
          h("button", { class: "btn small", type: "button", onclick: () => this.resolve(e, e.candidate) }, `归入 ${e.candidate} ${this.projectName(e.candidate)}`),
          h("button", { class: "btn small", type: "button", onclick: () => this.resolve(e, "") }, `新建「${e.proposed || e.title}」`),
        ),
      ),
    );
  }

  async resolve(e, project) {
    const r = await this.ctx
      .http("POST", `/v1/debug/xinli/resolve?${this.ctx.idQuery()}`, { session: e.session, exchange: e.id, project })
      .catch((err) => ({ status: 0, json: String(err) }));
    if (r.status !== 204) this.error = `确认失败：${r.status} ${r.json?.error ?? r.json ?? ""}`;
    this.refresh();
  }

  // —— 觉察 ——

  awareness() {
    const days = (this.state.days ?? []).filter((d) => d.awareness?.habits?.length || d.awareness?.suggestions?.length);
    const mems = this.memories;
    return [
      days.length
        ? days.map((d) =>
            h(
              "section",
              { class: "xinli-card" },
              h(
                "div",
                { class: "xinli-card-head" },
                h("b", {}, d.date),
                h("span", { class: "spacer" }),
                this.sessionLink(d.session, "打开每日整理会话"),
              ),
              list("沟通习惯", d.awareness.habits, "habit"),
              list("下次可以", d.awareness.suggestions, "suggestion"),
            ),
          )
        : h("div", { class: "empty" }, "还没有觉察：每日整理会从用户本人的话里总结沟通习惯与建议。"),
      h(
        "section",
        { class: "xinli-card" },
        h(
          "div",
          { class: "xinli-card-head" },
          h("b", {}, "长期记忆"),
          h("span", { class: "muted" }, " · 本 EndUser 的 Memory（含旁听写入的）"),
        ),
        mems.length
          ? h(
              "ul",
              { class: "xinli-memories" },
              mems.map((m) =>
                h(
                  "li",
                  {},
                  h("span", { class: `tag cat-${m.category}` }, m.category),
                  " ",
                  m.content,
                  h("small", { class: "muted" }, ` · ${ago(m.created_at)}`),
                ),
              ),
            )
          : h("div", { class: "muted" }, "还没有记忆。"),
      ),
    ];
  }

  sessionLink(sid, title) {
    if (!sid) return null;
    return h(
      "button",
      { class: "btn small", type: "button", title, onclick: (e) => (e.stopPropagation(), this.ctx.openSession(sid)) },
      "打开会话 →",
    );
  }
}

function list(label, items, cls = "") {
  if (!items?.length) return null;
  return h(
    "div",
    { class: `xinli-list ${cls}` },
    h("span", { class: "label" }, label),
    h(
      "ul",
      {},
      items.map((t) => h("li", {}, t)),
    ),
  );
}

/** 承诺列表：未核实的注明原因（不会按期限提醒）；withProject 时注明所属项目。 */
function commitmentList(label, items, withProject = false) {
  return h(
    "div",
    { class: "xinli-list commitments" },
    label ? h("span", { class: "label" }, label) : null,
    h(
      "ul",
      {},
      items.map((m) =>
        h(
          "li",
          { class: m.status !== "open" ? "muted" : "" },
          h("span", { class: `tag ${m.kind}` }, m.kind === "mine" ? "我" : "对方"),
          " ",
          m.content,
          m.counterpart ? h("small", {}, m.kind === "mine" ? ` · 答应 ${m.counterpart}` : ` · ${m.counterpart}`) : null,
          m.due_date || m.due_text ? h("small", { class: "due" }, ` · ${m.due_date || ""}${m.due_text ? `（${m.due_text}）` : ""}`) : null,
          withProject && m.project ? h("small", { class: "muted" }, ` · ${m.project}`) : null,
          m.status !== "open" ? h("small", {}, m.status === "done" ? " · 已完成" : " · 已取消") : null,
          m.unverified ? h("small", { class: "risk" }, ` · 未核实：${m.unverified}`) : null,
          h("small", { class: "muted" }, ` · ${m.id}`),
        ),
      ),
    ),
  );
}
