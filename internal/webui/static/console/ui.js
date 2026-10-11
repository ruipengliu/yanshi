// 控制台界面的小工具：建 DOM、格式化、读取工件。依赖 DOM。

export const $ = (id) => document.getElementById(id);

/**
 * h 创建元素：attrs 中 class、style（对象或字符串）、dataset、on<事件>（函数）、其余作为属性；children 可以是字符串、
 * 元素、数组或 null。
 */
export function h(tag, attrs = {}, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs ?? {})) {
    if (v === undefined || v === null || v === false) continue;
    if (k === "class") e.className = v;
    else if (k === "style" && typeof v === "object") {
      for (const [prop, val] of Object.entries(v)) {
        if (prop.startsWith("--")) e.style.setProperty(prop, String(val));
        else e.style[prop] = val;
      }
    } else if (k === "dataset") Object.assign(e.dataset, v);
    else if (k.startsWith("on") && typeof v === "function") e.addEventListener(k.slice(2), v);
    else if (k === "value") e.value = v;
    else if (v === true) e.setAttribute(k, "");
    else e.setAttribute(k, v);
  }
  append(e, children);
  return e;
}

/** fill 替换元素的全部子节点；与 h 一样跳过 null、展开数组（replaceChildren 会把 null 显示为文字）。 */
export function fill(e, ...children) {
  e.replaceChildren();
  append(e, children);
  return e;
}

function append(e, children) {
  for (const c of children) {
    if (c === null || c === undefined || c === false) continue;
    if (Array.isArray(c)) append(e, c);
    else e.append(c instanceof Node ? c : String(c));
  }
}

/** ID 的末尾一段，便于在界面上辨认。 */
export function short(id) {
  if (!id) return "";
  const s = String(id);
  return s.length > 14 ? "…" + s.slice(-8) : s;
}

export function clock(t) {
  const d = t instanceof Date ? t : new Date(t);
  return Number.isNaN(d.getTime()) ? "" : d.toLocaleTimeString("zh-CN", { hour12: false });
}

export function bytes(n) {
  n = Number(n ?? 0);
  if (n >= 1 << 20) return (n / (1 << 20)).toFixed(1) + " MB";
  if (n >= 1 << 10) return (n / (1 << 10)).toFixed(1) + " KB";
  return n + " B";
}

/** 稳定的颜色：同一个 ID 总是同一种颜色（区分 Run、Attempt、设备）。 */
export function hue(id) {
  let x = 0;
  for (const ch of String(id)) x = (x * 31 + ch.charCodeAt(0)) >>> 0;
  return x % 360;
}

export function toast(text, kind = "") {
  const box = document.getElementById("toasts");
  if (!box) return;
  const t = h("div", { class: `toast ${kind}` }, text);
  box.append(t);
  setTimeout(() => t.classList.add("out"), 3600);
  setTimeout(() => t.remove(), 4200);
}

/**
 * Artifacts 读取工件内容：-auth jwt 时下载要带令牌，<img src> 带不上，所以先下载成 blob 再给出对象 URL（按工件缓存）。
 */
export class Artifacts {
  constructor(httpBase, tokenFor) {
    this.httpBase = httpBase;
    this.tokenFor = tokenFor;
    this.cache = new Map();
  }

  /** uri 为 artifact://<id>；user 是有权读取它的 EndUser（其令牌）。 */
  url(uri, user) {
    const id = String(uri ?? "").replace(/^artifact:\/\//, "");
    if (!id) return Promise.reject(new Error("not an artifact"));
    if (!this.cache.has(id)) {
      const headers = {};
      const tok = this.tokenFor(user);
      if (tok) headers.Authorization = `Bearer ${tok}`;
      const p = fetch(`${this.httpBase}/v1/artifacts/${encodeURIComponent(id)}`, { headers }).then(async (r) => {
        if (!r.ok) throw new Error(`artifact ${id}: ${r.status}`);
        return URL.createObjectURL(await r.blob());
      });
      p.catch(() => this.cache.delete(id));
      this.cache.set(id, p);
    }
    return this.cache.get(id);
  }
}
