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


export function clock(t) {
  const d = t instanceof Date ? t : new Date(t);
  return Number.isNaN(d.getTime()) ? "" : d.toLocaleTimeString("zh-CN", { hour12: false });
}

export function ago(t) {
  const d = new Date(t);
  const s = (Date.now() - d.getTime()) / 1000;
  if (Number.isNaN(s)) return "";
  if (s < 60) return "刚刚";
  if (s < 3600) return `${Math.floor(s / 60)} 分钟前`;
  if (s < 86400 && new Date().getDate() === d.getDate()) return `${Math.floor(s / 3600)} 小时前`;
  return d.toLocaleDateString("zh-CN", { month: "numeric", day: "numeric" }) + " " + clock(d).slice(0, 5);
}
