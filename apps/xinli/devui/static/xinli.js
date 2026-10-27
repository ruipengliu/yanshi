// 旁听开发页面：展示 Persona 夹具，操作由本进程完成。
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
   *   fixturePath()        本进程夹具的身份路径
   *   consoleURL           内核控制台地址（不跨域读取内核）
   *   identity()           当前 EndUser 与业务线的标识：变化时丢弃在途的结果
   */
  constructor(ctx) {
    this.ctx = ctx;
    this.tab = "today";
    this.state = null;
    this.error = "";
    this.openProject = "";
    this.root = h("div", { class: "xinli" });
    this.timer = null;
  }

  /** 视图可见时每 3 秒刷新一次：每日整理与归集在后台进行。 */
  start() {
    if (this.timer) return;
    this.refresh();
    this.timer = setInterval(
      () => document.visibilityState === "visible" && this.refresh(),
      3000,
    );
  }

  stop() {
    clearInterval(this.timer);
    this.timer = null;
  }

  async refresh() {
    const who = this.ctx.identity();
    const st = await this.ctx
      .http("GET", `${this.ctx.fixturePath()}/state`)
      .catch((e) => ({ status: 0, json: String(e) }));
    if (who !== this.ctx.identity()) return;
    if (st.status === 200) {
      this.state = st.json;
      this.error = "";
    } else {
      this.state = null;
      this.error =
        st.status === 404
          ? "没有这个夹具路径，请检查业务线与 EndUser。"
          : `读取失败：${st.status} ${typeof st.json === "string" ? st.json : (st.json?.error ?? "")}`;
    }
    this.render();
  }

  render() {
    const body = this.error
      ? h("div", { class: "empty" }, this.error)
      : !this.state
        ? h("div", { class: "empty" }, "读取中…")
        : (
            {
              today: () => this.today(),
              projects: () => this.projects(),
              commitments: () => this.commitments(),
              pending: () => this.pending(),
            }[this.tab] ?? (() => this.awareness())
          )();
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
              {
                class: this.tab === id ? "selected" : "",
                onclick: () => ((this.tab = id), this.render()),
              },
              id === "pending" && this.pendingCount()
                ? `${name} ${this.pendingCount()}`
                : name,
            ),
          ),
        ),
        h(
          "span",
          { class: "muted" },
          "在对话中新建 xinli-day 会话，🎧 提交旁听后发「整理今天」；整理之后业务线为每个项目提交「整理 <日期>」，更新现状、对账承诺",
        ),
        h(
          "button",
          {
            class: "btn small",
            type: "button",
            title:
              "扮演业务线服务端：归档、提交「回顾本周」、向本周有交谈的项目提交「重写现状」",
            onclick: () => this.week(),
          },
          "每周回顾",
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
        this.exchangesOf((e) => e.session === d.session).map((e) =>
          this.exchange(e, true),
        ),
        this.mergedNote(d.session),
      ),
    );
  }

  /** 仍然有效的交谈（被当天整理并入别处的不单独列出）。 */
  exchangesOf(pred) {
    return (this.state.exchanges ?? []).filter((e) => !e.merged_into && pred(e));
  }

  /** 当天整理合并了几段被停顿切开的交谈时注明。 */
  mergedNote(session) {
    const merged = (this.state.exchanges ?? []).filter(
      (e) => e.session === session && e.merged_into,
    );
    if (!merged.length) return null;
    return h(
      "div",
      { class: "muted" },
      `当天整理合并了：${merged.map((e) => `${e.id} → ${e.merged_into}`).join("、")}`,
    );
  }

  projectName(id) {
    return (this.state.projects ?? []).find((p) => p.id === id)?.name ?? "";
  }

  /** 一段交谈；withWhere 时注明归属（可以几个项目），可跳到项目。 */
  exchange(e, withWhere) {
    let where = null;
    if (withWhere) {
      const links = (e.projects ?? []).filter(
        (l) => l.project || l.match === "unsure",
      );
      where = links.length
        ? links.map((l, i) => [
            i ? "、" : "",
            l.project
              ? h(
                  "a",
                  {
                    href: "#",
                    onclick: (ev) => (
                      ev.preventDefault(),
                      this.showProject(l.project)
                    ),
                  },
                  `${l.project} ${this.projectName(l.project)}`,
                )
              : h("span", { class: "tag" }, `待确认：可能属于 ${l.candidate}`),
          ])
        : h("span", { class: "muted" }, "不归项目");
    }
    const cms = (this.state.commitments ?? []).filter(
      (m) => m.session === e.session && m.exchange === e.id,
    );
    return h(
      "div",
      { class: "xinli-group" },
      h(
        "div",
        { class: "xinli-group-title" },
        h("b", {}, e.title),
        " ",
        h("code", {}, e.id),
        e.from ? h("small", { class: "muted" }, ` ${e.from}–${e.to}`) : null,
        where ? [" · ", where] : null,
      ),
      h("div", {}, e.summary),
      list(
        "已定",
        (e.decisions ?? []).map((d) =>
          d.project ? `${d.text}（${d.project}）` : d.text,
        ),
        "decided",
      ),
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
    if (!ps.length)
      return h(
        "div",
        { class: "empty" },
        "还没有项目。每日整理会为值得跟进的新主题建立项目。",
      );
    return ps.map((p) => {
      const open = this.openProject === p.id;
      const exs = this.exchangesOf((e) =>
        (e.projects ?? []).some((l) => l.project === p.id),
      );
      const cms = (this.state.commitments ?? []).filter(
        (m) => m.project === p.id && isOpen(m),
      );
      return h(
        "section",
        { class: `xinli-card ${open ? "open" : ""}` },
        h(
          "div",
          {
            class: "xinli-card-head",
            onclick: () => (
              (this.openProject = open ? "" : p.id),
              this.render()
            ),
          },
          h("b", {}, p.name),
          h("code", {}, p.id),
          p.archived ? h("span", { class: "tag" }, "已归档") : null,
          p.merged_into
            ? h("span", { class: "tag" }, `已并入 ${p.merged_into}`)
            : null,
          h(
            "span",
            { class: "muted" },
            ` · ${exs.length} 段交谈 · ${cms.length} 条未完成的承诺`,
          ),
          h("span", { class: "spacer" }),
          p.session
            ? this.sessionLink(p.session, "打开项目会话")
            : h("span", { class: "muted" }, "尚未归集"),
        ),
        h(
          "div",
          {},
          p.status || h("span", { class: "muted" }, "还没有现状"),
          p.status_at
            ? h("small", { class: "muted" }, ` · ${ago(p.status_at)}更新`)
            : null,
        ),
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
                .map((e) =>
                  h(
                    "div",
                    { class: "xinli-entry" },
                    h("div", { class: "xinli-entry-date" }, e.date),
                    this.exchange(e, false),
                  ),
                ),
            )
          : null,
      );
    });
  }

  // —— 承诺 ——

  commitments() {
    const cms = this.state.commitments ?? [];
    if (!cms.length)
      return h(
        "div",
        { class: "empty" },
        "还没有承诺。每日整理交出交谈时，双方各自答应的事会进入账本。",
      );
    const edit = (m, status) =>
      this.post("edit", { id: m.id, status });
    const groups = [
      ["我答应的", cms.filter((m) => m.kind === "mine" && isOpen(m))],
      ["别人答应我的", cms.filter((m) => m.kind === "theirs" && isOpen(m))],
      ["已完成、取消或合并", cms.filter((m) => !isOpen(m))],
    ];
    return groups
      .filter(([, list]) => list.length)
      .map(([label, list]) =>
        h(
          "section",
          { class: "xinli-card" },
          h("div", { class: "xinli-card-head" }, h("b", {}, label)),
          commitmentList("", list, true, edit),
        ),
      );
  }

  // —— 待确认 ——

  pendingList() {
    return this.exchangesOf((e) => !!pendingLink(e));
  }

  pendingMerges() {
    return (this.state.merges ?? []).filter((m) => m.status === "pending");
  }

  pendingCount() {
    return this.pendingList().length + this.pendingMerges().length;
  }

  pending() {
    const list = this.pendingList();
    const merges = this.pendingMerges();
    if (!list.length && !merges.length)
      return h(
        "div",
        { class: "empty" },
        "没有待确认的事。每日整理拿不准交谈的归属、每周回顾提议合并项目时，会放在这里请用户确认。",
      );
    return [
      merges.map((m) =>
        h(
          "section",
          { class: "xinli-card" },
          h(
            "div",
            { class: "xinli-card-head" },
            h(
              "b",
              {},
              `合并：${m.from} ${this.projectName(m.from)} → ${m.into} ${this.projectName(m.into)}`,
            ),
            h("span", { class: "spacer" }),
            this.sessionLink(m.session, "打开每周回顾会话"),
          ),
          h("div", {}, m.reason),
          h(
            "div",
            { class: "xinli-actions" },
            h(
              "button",
              {
                class: "btn small",
                type: "button",
                onclick: () =>
                  this.post("resolve_merge", { id: m.id, accept: true }),
              },
              "合并",
            ),
            h(
              "button",
              {
                class: "btn small",
                type: "button",
                onclick: () =>
                  this.post("resolve_merge", { id: m.id, accept: false }),
              },
              "不合并",
            ),
          ),
        ),
      ),
      list.map((e) =>
        h(
          "section",
          { class: "xinli-card" },
          h(
            "div",
            { class: "xinli-card-head" },
            h("b", {}, e.date),
            h("span", { class: "spacer" }),
            this.sessionLink(e.session, "打开每日整理会话"),
          ),
          this.exchange(e, false),
          h(
            "div",
            { class: "xinli-actions" },
            h(
              "button",
              {
                class: "btn small",
                type: "button",
                onclick: () => this.resolve(e, pendingLink(e).candidate),
              },
              `归入 ${pendingLink(e).candidate} ${this.projectName(pendingLink(e).candidate)}`,
            ),
            h(
              "button",
              {
                class: "btn small",
                type: "button",
                onclick: () => this.resolve(e, ""),
              },
              `新建「${pendingLink(e).proposed || e.title}」`,
            ),
          ),
        ),
      ),
    ];
  }

  resolve(e, project) {
    return this.post("resolve", {
      session: e.session,
      exchange: e.id,
      project,
    });
  }

  week() {
    return this.post("week_review", {});
  }

  /** 操作失败不立即刷新，以免成功的读取掩盖操作错误。 */
  async post(what, body) {
    const who = this.ctx.identity();
    const r = await this.ctx
      .http("POST", `${this.ctx.fixturePath()}/${what}`, body)
      .catch((err) => ({ status: 0, json: String(err) }));
    if (who !== this.ctx.identity()) return;
    if (r.status >= 300 || r.status === 0) {
      this.error = `操作失败：${r.status} ${r.json?.error ?? r.json ?? ""}`;
      this.render();
    } else {
      this.refresh();
    }
  }

  // —— 觉察 ——

  awareness() {
    const days = (this.state.days ?? []).filter(
      (d) => d.awareness?.habits?.length || d.awareness?.suggestions?.length,
    );
    const weeks = (this.state.weeks ?? []).slice().reverse();
    return [
      weeks.map((w) =>
        h(
          "section",
          { class: "xinli-card" },
          h(
            "div",
            { class: "xinli-card-head" },
            h("b", {}, "本周"),
            h("span", { class: "muted" }, ` · 回顾于 ${ago(w.filed_at)}`),
            h("span", { class: "spacer" }),
            this.sessionLink(w.session, "打开每周回顾会话"),
          ),
          list(
            "沟通习惯",
            (w.habits ?? []).map(
              (x) =>
                `${x.habit}（${(x.days ?? []).join("、")}）${x.example ? `：${x.example}` : ""}`,
            ),
            "habit",
          ),
          list("下周可以", w.suggestions, "suggestion"),
        ),
      ),
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
        : h(
            "div",
            { class: "empty" },
            "还没有觉察：每日整理会从用户本人的话里总结沟通习惯与建议。",
          ),
      h("p", { class: "muted" }, "旁听写入的长期 Memory 请在内核控制台的 Memory 页面查看。",
        this.ctx.consoleURL ? h("a", { href: this.ctx.consoleURL, target: "_blank", rel: "noopener" }, "打开控制台 →") : null),
    ];
  }

  sessionLink(sid, title) {
    if (!sid) return null;
    if (!this.ctx.consoleURL) return h("code", { title }, sid);
    return h("a", {
      class: "btn small", href: this.ctx.consoleURL, target: "_blank", rel: "noopener",
      title: `${title}：在控制台选择 ${sid}`,
      onclick: (e) => e.stopPropagation(),
    }, `${sid} · 打开控制台 →`);
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

/** 未完成且没有被合并的承诺：提醒只看这些。 */
function isOpen(m) {
  return m.status === "open" && !m.merged_into;
}

/** 交谈中归属拿不准、用户还没有确认的关联。 */
function pendingLink(e) {
  return (e.projects ?? []).find((l) => l.match === "unsure" && !l.project);
}

const OPS = { create: "记下", revise: "修订", close: "关闭", merge: "合并" };
const BASIS = {
  user: "用户",
  seed: "预置",
  "xinli-project": "依据摘要",
  "xinli-day": "依据原话",
};

/** 展开着修订历史的承诺：视图每 3 秒重绘一次，展开状态不随之丢失。 */
const openRevisions = new Set();

/** 修订历史：每次改动的说话时间、记录时间、依据与改动的字段。 */
function revisions(m) {
  const revs = m.revisions ?? [];
  if (revs.length < 2) return null;
  return h(
    "details",
    {
      class: "xinli-revisions",
      open: openRevisions.has(m.id),
      ontoggle: (ev) =>
        ev.target.open ? openRevisions.add(m.id) : openRevisions.delete(m.id),
    },
    h("summary", {}, `${revs.length} 条修订`),
    h(
      "ol",
      {},
      revs.map((r) =>
        h(
          "li",
          {},
          `${OPS[r.op] ?? r.op}`,
          r.fields
            ? ` ${Object.entries(r.fields)
                .map(([k, v]) => `${k}=${v || "（清空）"}`)
                .join("，")}`
            : "",
          h(
            "small",
            { class: "muted" },
            ` · ${BASIS[r.author] ?? r.author}${(r.refs ?? []).length ? ` ${r.refs.join("、")}` : ""}${(r.exchanges ?? []).length ? ` ${r.exchanges.join("、")}` : ""}` +
              `${r.spoken_at ? ` · 说于 ${new Date(r.spoken_at).toLocaleString()}` : ""} · 记于 ${ago(r.recorded_at)}` +
              `${r.reason ? ` · ${r.reason}` : ""}`,
          ),
        ),
      ),
    ),
  );
}

/** 承诺列表：未核实的注明原因（不会按期限提醒）；withProject 时注明所属项目；edit 非空时可替用户标为完成或取消。 */
function commitmentList(label, items, withProject = false, edit = null) {
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
          { class: isOpen(m) ? "" : "muted" },
          h(
            "span",
            { class: `tag ${m.kind}` },
            m.kind === "mine" ? "我" : "对方",
          ),
          " ",
          m.content,
          m.counterpart
            ? h(
                "small",
                {},
                m.kind === "mine"
                  ? ` · 答应 ${m.counterpart}`
                  : ` · ${m.counterpart}`,
              )
            : null,
          m.due_date || m.due_text
            ? h(
                "small",
                { class: "due" },
                ` · ${m.due_date || ""}${m.due_text ? `（${m.due_text}）` : ""}`,
              )
            : null,
          withProject && m.project
            ? h("small", { class: "muted" }, ` · ${m.project}`)
            : null,
          m.merged_into
            ? h("small", {}, ` · 已并入 ${m.merged_into}`)
            : m.status !== "open"
              ? h("small", {}, m.status === "done" ? " · 已完成" : " · 已取消")
              : null,
          (m.locked ?? []).length
            ? h("small", { class: "muted" }, " · 用户改过")
            : null,
          m.unverified
            ? h("small", { class: "risk" }, ` · 未核实：${m.unverified}`)
            : null,
          h("small", { class: "muted" }, ` · ${m.id} rev ${(m.revisions ?? []).length}`),
          edit && isOpen(m)
            ? [
                " ",
                h(
                  "button",
                  { class: "btn small", type: "button", onclick: () => edit(m, "done") },
                  "完成",
                ),
                h(
                  "button",
                  { class: "btn small", type: "button", onclick: () => edit(m, "cancelled") },
                  "取消",
                ),
              ]
            : null,
          revisions(m),
        ),
      ),
    ),
  );
}
