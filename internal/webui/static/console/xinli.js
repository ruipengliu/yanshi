// 旁听视图（docs/design/console.md §2.1）：旁听业务线 xinli 这一侧的数据，按 App 的三个页面展示——
//   今日   每次每日整理的总览、要点，以及各项目分组中定下的事、双方的待办与风险
//   项目   跨天归集的项目：近况、按天的时间线、用户本人的承诺（xinli-project 的 set_commitments）
//   觉察   用户本人的沟通习惯与建议（按天），以及旁听写入的长期 Memory
// 数据来自模拟的 xinli MCP Server（serve -xinli-mock，GET /v1/debug/xinli），可跳到对应的 Session。

import { ago, fill, h } from "./ui.js";

const TABS = [
  ["today", "今日"],
  ["projects", "项目"],
  ["awareness", "觉察"],
];

export class XinliView {
  /**
   * @param {object} ctx
   *   http(method, path)   调用 HTTP API，返回 {status, json}
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
        : this.tab === "today"
          ? this.today()
          : this.tab === "projects"
            ? this.projects()
            : this.awareness();
    fill(
      this.root,
      h(
        "div",
        { class: "xinli-head" },
        h(
          "nav",
          { class: "segmented" },
          TABS.map(([id, name]) =>
            h("button", { class: this.tab === id ? "selected" : "", onclick: () => ((this.tab = id), this.render()) }, name),
          ),
        ),
        h(
          "span",
          { class: "muted" },
          "在对话中新建 xinli-day 会话，🎧 提交旁听后发「整理今天」；整理之后业务线为每个项目提交「整理 <日期>」，得出承诺",
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
        (d.groups ?? []).map((g) => this.group(g, true)),
      ),
    );
  }

  group(g, withProject) {
    return h(
      "div",
      { class: "xinli-group" },
      h(
        "div",
        { class: "xinli-group-title" },
        withProject
          ? h("a", { href: "#", onclick: (e) => (e.preventDefault(), this.showProject(g.project)) }, `${g.project} ${g.title}`)
          : h("b", {}, g.title),
      ),
      h("div", {}, g.summary),
      list("已定", g.decisions, "decided"),
      list("我要做", g.my_todos, "mine"),
      list("对方要做", g.their_todos, "theirs"),
      list("风险", g.risks, "risk"),
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
      return h(
        "section",
        { class: `xinli-card ${open ? "open" : ""}` },
        h(
          "div",
          { class: "xinli-card-head", onclick: () => ((this.openProject = open ? "" : p.id), this.render()) },
          h("b", {}, p.name),
          h("code", {}, p.id),
          h("span", { class: "muted" }, ` · ${(p.timeline ?? []).length} 天 · ${(p.commitments ?? []).length} 条承诺`),
          h("span", { class: "spacer" }),
          p.session ? this.sessionLink(p.session, "打开项目会话") : h("span", { class: "muted" }, "尚未归集"),
        ),
        h("div", {}, p.digest),
        (p.commitments ?? []).length
          ? h(
              "div",
              { class: "xinli-list commitments" },
              h("span", { class: "label" }, `承诺${p.commitments_at ? `（${ago(p.commitments_at)}更新）` : ""}`),
              h(
                "ul",
                {},
                p.commitments.map((m) =>
                  h(
                    "li",
                    {},
                    m.content,
                    m.to ? h("small", {}, ` · 答应 ${m.to}`) : null,
                    m.due ? h("small", { class: "due" }, ` · ${m.due}`) : null,
                  ),
                ),
              ),
            )
          : null,
        open
          ? h(
              "div",
              { class: "xinli-timeline" },
              (p.timeline ?? [])
                .slice()
                .reverse()
                .map((e) =>
                  h("div", { class: "xinli-entry" }, h("div", { class: "xinli-entry-date" }, e.date), this.group(e, false)),
                ),
            )
          : null,
      );
    });
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
