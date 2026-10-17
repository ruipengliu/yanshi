// 卡片、提问与审批的渲染（docs/design/cards.md §5，参考实现）。卡片的数据是 protojson 的 Card（snake_case）。

import { render as markdown } from "./markdown.js";
import { h } from "./ui.js";

/** 内置图标（card.Icons）。 */
export const ICONS = {
  alarm: "⏰",
  calendar: "📅",
  car: "🚗",
  check: "✅",
  clock: "🕒",
  cloud: "☁️",
  file: "📄",
  heart: "❤️",
  home: "🏠",
  info: "ℹ️",
  location: "📍",
  mail: "✉️",
  message: "💬",
  music: "🎵",
  phone: "📞",
  photo: "🖼️",
  search: "🔍",
  shopping: "🛍️",
  star: "⭐",
  timer: "⏱️",
  user: "👤",
  warning: "⚠️",
  weather: "🌤️",
};

/**
 * image 渲染卡片中的图片地址：icon:<名称> 为内置图标，artifact://<id> 经 ctx.image 读取（工件可能需要令牌）。
 */
export function image(uri, alt, ctx, cls = "") {
  if (!uri) return null;
  if (uri.startsWith("icon:")) return h("span", { class: `card-icon ${cls}`, title: uri }, ICONS[uri.slice(5)] ?? "▫️");
  return ctx.image ? ctx.image(uri, alt, cls) : h("span", { class: "md-blocked" }, alt || uri);
}

/**
 * renderCard 渲染一张卡片。ctx：
 *   taken      已执行过的动作（"<call_id>/<action>"），对应的按钮显示为已选，整张卡片不再接受动作；
 *   onAction(action, values, label) 提交动作，返回 Promise；
 *   image(uri, alt, cls)、link(uri) 读取工件；
 *   history    同一 card_id 被替换掉的旧版本数（显示"已更新"）；
 *   values     字段的填写值（字段 ID → 值），由调用方按卡片保存：同一 Run 的后续消息会重建卡片，重建时据此恢复已填写的值。
 */
export function renderCard(card, ctx = {}) {
  const taken = ctx.taken ?? new Set();
  // 任一动作执行过，整张卡片就不再接受动作（同一版卡片上的动作只生效一次，换一个动作也不行）。
  const done = [...(card.buttons ?? []).map((b) => b.id), ...listActions(card)].some((a) => taken.has(`${card.call_id}/${a}`));
  const values = ctx.values ?? {};
  const root = h("div", { class: `ycard ${done ? "done" : ""}`, dataset: { card: card.card_id } });
  const head = h(
    "div",
    { class: "ycard-head" },
    image(card.image, card.title, ctx, "hero"),
    h(
      "div",
      { class: "ycard-titles" },
      h("div", { class: "ycard-title" }, card.title ?? ""),
      card.subtitle ? h("div", { class: "ycard-sub" }, card.subtitle) : null,
    ),
    ctx.history ? h("span", { class: "pill", title: `同一 card_id 已展示 ${ctx.history + 1} 版，显示最新版` }, "已更新") : null,
  );
  root.append(head);
  const act = async (action, label, btn) => {
    if (done || root.classList.contains("busy")) return;
    root.classList.add("busy");
    btn?.classList.add("chosen");
    try {
      await ctx.onAction?.(action, { ...values }, label);
    } catch (e) {
      btn?.classList.remove("chosen");
      root.append(h("div", { class: "ycard-error" }, e.message));
    } finally {
      root.classList.remove("busy");
    }
  };
  for (const el of card.body ?? []) {
    if (el.markdown) root.append(h("div", { class: "md" }, markdown(el.markdown.text, ctx)));
    else if (el.image)
      root.append(
        h(
          "figure",
          { class: "ycard-figure" },
          image(el.image.uri, el.image.alt, ctx, "wide"),
          el.image.alt ? h("figcaption", {}, el.image.alt) : null,
        ),
      );
    else if (el.facts)
      root.append(
        h(
          "dl",
          { class: "facts" },
          (el.facts.facts ?? []).map((f) => [h("dt", {}, f.label), h("dd", {}, f.value)]),
        ),
      );
    else if (el.list) {
      root.append(
        h(
          "div",
          { class: "ylist" },
          (el.list.items ?? []).map((it) => {
            const chosen = it.action && taken.has(`${card.call_id}/${it.action}`);
            const row = h(
              it.action ? "button" : "div",
              { class: `ylist-item ${it.action ? "actionable" : ""} ${chosen ? "chosen" : ""}`, disabled: it.action && done },
              image(it.image, it.title, ctx, "thumb"),
              h(
                "div",
                {},
                h("div", { class: "ylist-title" }, it.title),
                it.subtitle ? h("div", { class: "ylist-sub" }, it.subtitle) : null,
              ),
              it.action ? h("span", { class: "chev" }, chosen ? "✓" : "›") : null,
            );
            if (it.action) row.addEventListener("click", () => act(it.action, it.title, row));
            return row;
          }),
        ),
      );
    } else if (el.field) root.append(field(el.field, values, done));
  }
  if (card.buttons?.length) {
    root.append(
      h(
        "div",
        { class: "ycard-actions" },
        card.buttons.map((b) => {
          const chosen = taken.has(`${card.call_id}/${b.id}`);
          const btn = h(
            "button",
            { class: `${b.style || "default"} ${chosen ? "chosen" : ""}`, disabled: done },
            chosen ? `✓ ${b.label}` : b.label,
          );
          btn.addEventListener("click", () => act(b.id, b.label, btn));
          return btn;
        }),
      ),
    );
  }
  return root;
}

function listActions(card) {
  const out = [];
  for (const el of card.body ?? []) for (const it of el.list?.items ?? []) if (it.action) out.push(it.action);
  return out.length ? out : [];
}

/**
 * field 渲染一个字段。values 中已有该字段的值（重建前填写的）时显示它，否则以字段的初始值填入 values：提交的是界面上
 * 看到的值。开关没有初始值时是关闭，同样记为 "false"，否则必填的开关保持关闭就提交不了。
 */
function field(f, values, disabled) {
  const id = f.id;
  if (values[id] === undefined) {
    if (f.type === "toggle") values[id] = String(f.value === "true");
    else if (f.value) values[id] = f.value;
  }
  const cur = values[id] ?? "";
  const set = (v) => (values[id] = v);
  let input;
  switch (f.type) {
    case "select":
      input = h(
        "select",
        { disabled, onchange: (e) => set(e.target.value) },
        h("option", { value: "" }, "请选择"),
        (f.options ?? []).map((o) => h("option", { value: o, selected: o === cur }, o)),
      );
      break;
    case "toggle":
      input = h("input", { type: "checkbox", disabled, checked: cur === "true", onchange: (e) => set(String(e.target.checked)) });
      break;
    default:
      input = h("input", {
        type: { number: "number", date: "date", time: "time" }[f.type] ?? "text",
        value: cur,
        disabled,
        oninput: (e) => set(e.target.value),
      });
  }
  return h("label", { class: "yfield" }, h("span", {}, f.label, f.required ? h("b", { class: "req" }, " *") : null), input);
}

/**
 * renderQuestion 以卡片样式渲染 ask_user 提问（ADR-0025，docs/design/cards.md §5）：选项是可点选的条目（带说明与配图），
 * 字段与"提交"在下方。onAnswer({selected, values, text}) 提交回答。
 *
 * 单选且没有字段时点选即提交；有字段时点选只是选中（单选互斥），由"提交"把选项、字段与文字一起发出，否则选了选项就
 * 来不及填字段。ctx.state（{values, selected, text}）由调用方按提问保存，重建时恢复已选与已填写的内容。
 */
export function renderQuestion(q, ctx = {}) {
  const state = ctx.state ?? { values: {}, selected: new Set(), text: "" };
  const { values, selected } = state;
  const instant = !q.multiple && !q.fields?.length;
  const root = h("div", { class: "ycard question" });
  root.append(
    h(
      "div",
      { class: "ycard-head" },
      h("span", { class: "card-icon hero" }, "❓"),
      h(
        "div",
        { class: "ycard-titles" },
        h("div", { class: "ycard-title" }, q.question),
        h("div", { class: "ycard-sub" }, q.agent ? `子 Agent ${q.agent} 在等你回答` : "Agent 在等你回答"),
      ),
    ),
  );
  const send = async (a) => {
    root.classList.add("busy");
    try {
      await ctx.onAnswer?.(a);
    } catch (e) {
      root.append(h("div", { class: "ycard-error" }, e.message));
    } finally {
      root.classList.remove("busy");
    }
  };
  if (q.options?.length) {
    const mark = (id) => (instant ? "›" : q.multiple ? (selected.has(id) ? "☑" : "□") : selected.has(id) ? "◉" : "○");
    const rows = q.options.map((o) => {
      const row = h(
        "button",
        { class: `ylist-item actionable ${selected.has(o.id) ? "chosen" : ""}`, dataset: { option: o.id } },
        image(o.image, o.label, ctx, "thumb"),
        h("div", {}, h("div", { class: "ylist-title" }, o.label), o.description ? h("div", { class: "ylist-sub" }, o.description) : null),
        h("span", { class: "chev" }, mark(o.id)),
      );
      row.addEventListener("click", () => {
        if (instant) return send({ selected: [o.id] });
        if (q.multiple) selected.has(o.id) ? selected.delete(o.id) : selected.add(o.id);
        else {
          const on = !selected.has(o.id);
          selected.clear();
          if (on) selected.add(o.id);
        }
        for (const r of rows) {
          const id = r.dataset.option;
          r.classList.toggle("chosen", selected.has(id));
          r.querySelector(".chev").textContent = mark(id);
        }
      });
      return row;
    });
    root.append(h("div", { class: "ylist" }, rows));
  }
  for (const f of q.fields ?? []) root.append(field({ id: f.name, label: f.label, type: f.type, required: f.required }, values, false));
  const text = h("input", { placeholder: "或直接输入回答…", class: "yanswer-text", value: state.text });
  text.addEventListener("input", () => (state.text = text.value));
  root.append(
    h(
      "div",
      { class: "ycard-actions" },
      text,
      h("button", { class: "primary", onclick: () => send({ selected: [...selected], values, text: text.value.trim() }) }, "提交"),
    ),
  );
  return root;
}

/** renderApproval 渲染待审批的调用：onDecide(approve)。 */
export function renderApproval(a, ctx = {}) {
  const root = h("div", { class: "ycard approval" });
  root.append(
    h(
      "div",
      { class: "ycard-head" },
      h("span", { class: "card-icon hero" }, "🛡️"),
      h(
        "div",
        { class: "ycard-titles" },
        h("div", { class: "ycard-title" }, "需要你确认"),
        h("div", { class: "ycard-sub" }, a.agent ? `子 Agent ${a.agent} · ${a.capability}` : a.capability),
      ),
    ),
    h("div", { class: "md" }, a.summary || a.capability),
  );
  const decide = async (ok) => {
    root.classList.add("busy");
    try {
      await ctx.onDecide?.(ok);
    } catch (e) {
      root.append(h("div", { class: "ycard-error" }, e.message));
    } finally {
      root.classList.remove("busy");
    }
  };
  root.append(
    h(
      "div",
      { class: "ycard-actions" },
      h("button", { class: "danger", onclick: () => decide(false) }, "拒绝"),
      h("button", { class: "primary", onclick: () => decide(true) }, "批准"),
    ),
  );
  return root;
}
