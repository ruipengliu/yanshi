import { fill, h } from "./ui.js";

// 表单独立于整理结果刷新，轮询不会吞掉正在输入的文字。
export class CaptureView {
  constructor(ctx) {
    this.ctx = ctx;
    this.version = 0;
    const now = new Date();
    const today = `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, "0")}-${String(now.getDate()).padStart(2, "0")}`;
    this.date = h("input", { type: "date", value: today, onchange: () => this.reset() });
    this.lines = h("textarea", { rows: 5, placeholder: "佩戴者|我明天给你报价\n他人 colleague @P-0012|好的" });
    this.from = h("input", { placeholder: "起始引用，如 #1.0" });
    this.to = h("input", { placeholder: "结束引用，如 #1.1" });
    this.notice = h("p", { role: "status" });
    this.results = h("div");
    this.busy = false;
    this.buttons = [];
    const button = (label, fn) => {
      const b = h("button", { class: "btn small", type: "button", onclick: () => this.action(fn) }, label);
      this.buttons.push(b);
      return b;
    };
    this.root = h("section", { class: "xinli-card capture-entry" },
      h("div", { class: "xinli-card-head" }, h("b", {}, "提交旁听、原文与删除"), h("label", {}, "业务日 ", this.date)),
      h("p", { class: "muted" }, "逐行输入「佩戴者|文字」或「他人 key|文字」，他人未填 key 时使用 other；可在 key 后加 @说话人引用，如「他人 colleague @P-0012|文字」。"),
      this.lines,
      h("div", { class: "xinli-actions" },
        button("提交旁听", () => this.append()),
        ctx.orchestration ? button("整理交谈", () => this.organize("exchange")) : null,
        ctx.orchestration ? button("整理今天", () => this.organize("day")) : null),
      h("div", { class: "xinli-actions" }, this.from, this.to,
        button("擦除", () => this.send("erase", { from: this.from.value, to: this.to.value })),
        button("删除这一天", () => this.send("erase_day", {}))),
      this.notice, this.results);
  }

  path() { return `${this.ctx.devPath()}/captures?${new URLSearchParams({ date: this.date.value })}`; }
  identity() { return `${this.ctx.identity()}/${this.date.value}`; }
  reset() {
    this.version++;
    this.lines.value = "";
    this.from.value = this.to.value = "";
    this.notice.textContent = "";
    fill(this.results);
    this.refresh();
  }
  start() {
    this.refresh();
    this.timer = setInterval(() => document.visibilityState === "visible" && this.refresh(), 3000);
  }
  stop() { clearInterval(this.timer); this.version++; }

  async refresh() {
    const who = this.identity();
    const version = ++this.version;
    try {
      const r = await this.ctx.http("GET", this.path());
      if (who !== this.identity() || version !== this.version) return;
      if (r.status !== 200) { fill(this.results, "读取失败，请检查业务日后重试。"); return; }
      const data = r.json;
      fill(this.results,
        h("h3", {}, data.day.deleted ? "原文（这一天已删除）" : "原文"),
        (data.segments ?? []).length ? data.segments.map((s) => h("div", { class: "xinli-group capture-text" },
          h("code", {}, `#${s.ref}`), " · ", s.speaker === "wearer" ? "佩戴者" : `他人 ${s.speaker_key}${s.speaker_ref ? ` @${s.speaker_ref}` : ""}`,
          " · ", s.spoken_at || "未记时刻", h("div", {}, s.text))) : h("p", { class: "muted" }, "没有旁听原文。"),
        h("h3", {}, "删除待办"),
        (data.tasks ?? []).length ? data.tasks.map((task) => h("div", { class: "xinli-group" },
          h("b", {}, task.status === "completed" ? "已完成" : "删除中"), " · ", h("code", {}, task.id),
          task.rebuild_pending ? h("span", { class: "muted" }, " · 等待重新整理") : null,
          task.error ? h("p", {}, `最近错误类别：${task.error}`) : null,
          task.status === "completed" && task.candidates?.length ? [h("p", {}, "可能受影响的 Memory（保留，供用户决定）："),
            task.candidates.map((m) => h("div", { class: "capture-text" }, h("code", {}, m.id), " · ", m.summary))] : null))
          : h("p", { class: "muted" }, "没有删除待办。"));
    } catch {
      if (who === this.identity() && version === this.version) fill(this.results, "读取失败，请稍后重试。");
    }
  }

  async action(fn) {
    if (this.busy) return;
    const who = this.identity();
    this.busy = true;
    this.buttons.forEach((b) => b.disabled = true);
    try { await fn(); }
    catch { if (who === this.identity()) this.notice.textContent = "操作失败，请检查输入或稍后重试。"; }
    finally { this.busy = false; this.buttons.forEach((b) => b.disabled = false); }
  }

  async send(action, body) {
    const who = this.identity();
    const r = await this.ctx.http("POST", `${this.ctx.devPath()}/${action}`, { date: this.date.value, ...body });
    if (who !== this.identity()) return null;
    if (r.status !== 200) { this.notice.textContent = `操作失败：${r.json?.error || r.status}`; return null; }
    this.notice.textContent = r.json.refs ? `已提交：${r.json.refs.map((ref) => `#${ref}`).join("、")}`
      : r.json.session_id ? `已提交整理：${r.json.session_id}` : "删除请求已记录。";
    await this.refresh();
    return who === this.identity() ? r.json : null;
  }

  async append() {
    const zone = Intl.DateTimeFormat().resolvedOptions().timeZone || "Asia/Shanghai";
    const spoken = new Date();
    const [year, month, day] = this.date.value.split("-").map(Number);
    spoken.setFullYear(year, month - 1, day);
    const segments = this.lines.value.split("\n").filter((line) => line.trim()).map((line) => {
      const sep = line.indexOf("|");
      if (sep < 0) throw new Error("invalid line");
      const who = line.slice(0, sep).trim();
      const text = line.slice(sep + 1).trim();
      const other = /^他人(?:\s+([A-Za-z0-9_-]{1,32}))?(?:\s+@([A-Za-z0-9_-]{1,64}))?$/.exec(who);
      if (!text || (who !== "佩戴者" && !other)) throw new Error("invalid speaker");
      return { speaker: other ? "other" : "wearer", speaker_key: other ? other[1] || "other" : "",
        ...(other?.[2] ? { speaker_ref: other[2] } : {}),
        text, spoken_at: spoken.toISOString(), time_zone: zone };
    });
    if (await this.send("captures", { segments })) this.lines.value = "";
  }

  organize(kind) {
    const bytes = crypto.getRandomValues(new Uint8Array(16));
    const input_id = `xinli_dev_${Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("")}`;
    return this.send("organize", { kind, input_id });
  }
}
