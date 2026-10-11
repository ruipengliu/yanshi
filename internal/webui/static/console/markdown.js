// 安全的 Markdown 子集（docs/design/cards.md §2）：标题、段落、粗体、斜体、行内代码、代码块、列表、引用、表格、
// 分隔线、链接与图片。直接构造 DOM、不经 innerHTML，模型输出中的 HTML 原样显示为文字；链接只允许 https: 与
// artifact://（工件经 resolve 读取），图片只允许 artifact://。

import { h } from "./ui.js";

/**
 * render 把 Markdown 文本渲染为 DocumentFragment。opts.image(uri, alt) 返回图片元素（工件需要带令牌读取），
 * opts.link(uri) 处理 artifact:// 链接的点击。
 */
export function render(text, opts = {}) {
  const frag = document.createDocumentFragment();
  const lines = String(text ?? "")
    .replace(/\r\n?/g, "\n")
    .split("\n");
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (/^\s*$/.test(line)) {
      i++;
      continue;
    }
    // 代码块
    const fence = /^\s*```(\w*)/.exec(line);
    if (fence) {
      const body = [];
      i++;
      while (i < lines.length && !/^\s*```/.test(lines[i])) body.push(lines[i++]);
      i++;
      frag.append(h("pre", { class: "md-code" }, h("code", {}, body.join("\n"))));
      continue;
    }
    const head = /^(#{1,4})\s+(.*)$/.exec(line);
    if (head) {
      frag.append(h(`h${Math.min(6, head[1].length + 2)}`, { class: "md-h" }, inline(head[2], opts)));
      i++;
      continue;
    }
    if (/^\s*([-*_])\s*\1\s*\1[\s\-*_]*$/.test(line)) {
      frag.append(h("hr"));
      i++;
      continue;
    }
    if (/^\s*>/.test(line)) {
      const body = [];
      while (i < lines.length && /^\s*>/.test(lines[i])) body.push(lines[i++].replace(/^\s*>\s?/, ""));
      frag.append(h("blockquote", {}, render(body.join("\n"), opts)));
      continue;
    }
    if (/^\s*\|.*\|\s*$/.test(line) && i + 1 < lines.length && /^\s*\|[\s:|-]+\|\s*$/.test(lines[i + 1])) {
      const cells = (l) =>
        l
          .trim()
          .replace(/^\||\|$/g, "")
          .split("|")
          .map((c) => c.trim());
      const header = cells(line);
      i += 2;
      const rows = [];
      while (i < lines.length && /^\s*\|.*\|\s*$/.test(lines[i])) rows.push(cells(lines[i++]));
      frag.append(
        h(
          "table",
          { class: "md-table" },
          h(
            "thead",
            {},
            h(
              "tr",
              {},
              header.map((c) => h("th", {}, inline(c, opts))),
            ),
          ),
          h(
            "tbody",
            {},
            rows.map((r) =>
              h(
                "tr",
                {},
                r.map((c) => h("td", {}, inline(c, opts))),
              ),
            ),
          ),
        ),
      );
      continue;
    }
    const item = /^(\s*)([-*+]|\d+[.)])\s+(.*)$/;
    if (item.test(line)) {
      const ordered = /\d/.test(item.exec(line)[2]);
      const list = h(ordered ? "ol" : "ul");
      while (i < lines.length && item.test(lines[i])) {
        const m = item.exec(lines[i]);
        const task = /^\[( |x|X)\]\s+(.*)$/.exec(m[3]);
        list.append(
          h(
            "li",
            {},
            task
              ? [h("input", { type: "checkbox", disabled: true, checked: task[1] !== " " }), " ", inline(task[2], opts)]
              : inline(m[3], opts),
          ),
        );
        i++;
      }
      frag.append(list);
      continue;
    }
    const para = [];
    while (i < lines.length && !/^\s*$/.test(lines[i]) && !/^\s*(```|#{1,4}\s|>|[-*+]\s|\d+[.)]\s|\|)/.test(lines[i]))
      para.push(lines[i++]);
    if (!para.length) para.push(lines[i++]);
    const p = h("p");
    para.forEach((l, k) => {
      if (k) p.append(h("br"));
      p.append(inline(l, opts));
    });
    frag.append(p);
  }
  return frag;
}

// 强调只认 * 与 **：下划线常见于工具名与标识符（show_card），按强调解析会吞掉它们。
const TOKEN = /(`[^`]+`)|(\*\*[^*]+\*\*)|(\*[^*\s][^*]*\*)|(!\[[^\]]*\]\([^)\s]+\))|(\[[^\]]+\]\([^)\s]+\))|(https:\/\/[^\s<>()]+)/;

function inline(text, opts) {
  const frag = document.createDocumentFragment();
  let s = String(text ?? "");
  while (s) {
    const m = TOKEN.exec(s);
    if (!m) {
      frag.append(s);
      break;
    }
    if (m.index) frag.append(s.slice(0, m.index));
    const t = m[0];
    if (m[1]) frag.append(h("code", {}, t.slice(1, -1)));
    else if (m[2]) frag.append(h("strong", {}, inline(t.slice(2, -2), opts)));
    else if (m[3]) frag.append(h("em", {}, inline(t.slice(1, -1), opts)));
    else if (m[4]) {
      const [, alt, uri] = /^!\[([^\]]*)\]\(([^)\s]+)\)$/.exec(t);
      frag.append(
        uri.startsWith("artifact://") && opts.image
          ? opts.image(uri, alt)
          : h("span", { class: "md-blocked", title: "只显示本会话的工件图片" }, `[图片：${alt || uri}]`),
      );
    } else if (m[5]) {
      const [, label, uri] = /^\[([^\]]+)\]\(([^)\s]+)\)$/.exec(t);
      frag.append(link(label, uri, opts));
    } else if (m[6]) frag.append(link(t, t, opts));
    s = s.slice(m.index + t.length);
  }
  return frag;
}

function link(label, uri, opts) {
  if (uri.startsWith("https://")) return h("a", { href: uri, target: "_blank", rel: "noopener noreferrer" }, label);
  if (uri.startsWith("artifact://")) return h("a", { href: "#", onclick: (e) => (e.preventDefault(), opts.link?.(uri)) }, label);
  return h("span", { class: "md-blocked", title: "只允许 https: 与 artifact:// 链接" }, label);
}
