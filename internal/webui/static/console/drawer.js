// 详情抽屉："对话"视图右侧，显示选中的调用、模型调用或卡片的全部信息（docs/design/console.md §2）。
//   调用      状态、所属 Attempt、路由目标（设备、子 Session、用户本人）、执行次数、审批；各步的时间（相对模型发起调用）；
//            参数与结果。
//   模型调用  模型、token（输入、缓存、输出、思考）、首字延迟与耗时、结束原因；思考过程与回复全文；发起的调用。
//   卡片      每一版（调用、时间）与已执行的动作；最新一版的 JSON。

import { callIndex, callState, modelLine, toolLabel } from "./chat.js";
import { cardActionOf, payloadOf, textOf } from "./device.js";
import { clock, dur, h, jsonView, num, short } from "./ui.js";

const STATE = {
  queued: ["排队", ""],
  running: ["执行中", "warn"],
  approval: ["待审批", "warn"],
  denied: ["已拒绝", "bad"],
  error: ["出错", "bad"],
  done: ["完成", "ok"],
  // 所属 Run 已中断或失败，这个调用不会再有结果。
  aborted: ["已终止", ""],
  // 后台调用（ADR-0035）：设备离线超过宽限期，Run 不再等它。
  background: ["后台排队中", "warn"],
  expired: ["已到期，未执行", ""],
  cancelled: ["已取消", ""],
};

/**
 * renderDrawer 返回抽屉内容。ctx：{view, childView(sid), openSession(sid), onSelect(sel), onTimeline(runId), close()}。
 */
export function renderDrawer(sel, ctx) {
  const v = ctx.view;
  if (!sel || !v)
    return shell("详情", null, ctx, h("div", { class: "drawer-empty" }, "点对话中的调用、模型信息行或卡片，在这里查看详情。"));
  if (sel.kind === "call") return callDetail(sel.callId, ctx);
  if (sel.kind === "model") return modelDetail(sel.seq, ctx);
  if (sel.kind === "card") return cardDetail(sel.cardId, ctx);
  return shell("详情", null, ctx, null);
}

function shell(title, sub, ctx, ...body) {
  return h(
    "div",
    { class: "drawer-inner" },
    h(
      "div",
      { class: "drawer-header" },
      h("div", { class: "drawer-title" }, title, sub ? h("code", {}, " ", sub) : null),
      h("button", { class: "icon-btn", title: "关闭", onclick: () => ctx.close() }, "✕"),
    ),
    h("div", { class: "drawer-body" }, body),
  );
}

function section(title, extra, ...body) {
  return h("section", { class: "detail-section" }, h("div", { class: "section-heading" }, h("span", {}, title), extra), body);
}

function overview(rows) {
  return h(
    "dl",
    { class: "overview" },
    rows.filter(Boolean).map(([k, v]) => [h("dt", {}, k), h("dd", {}, v)]),
  );
}

function pill(state) {
  const [text, cls] = STATE[state] ?? [state, ""];
  return h("span", { class: `status-pill ${cls}` }, text);
}

function steps(list) {
  return h(
    "ol",
    { class: "steps" },
    list
      .filter(Boolean)
      .map(([name, t, t0, cls]) =>
        h(
          "li",
          { class: cls ?? "" },
          h("span", {}, name),
          h("time", {}, t0 !== undefined && t ? `+${((t - t0) / 1000).toFixed(1)}s` : t ? clock(t) : ""),
        ),
      ),
  );
}

function callDetail(callId, ctx) {
  const v = ctx.view;
  const st = callIndex(v).get(callId);
  if (!st) return shell("调用", short(callId), ctx, h("div", { class: "drawer-empty" }, "日志中没有这个调用"));
  const l = toolLabel(st.capability);
  const state = callState(st);
  const node = st.nodeId ?? "";
  const child = node.startsWith("@agent/") ? node.slice(7) : "";
  const cv = child ? ctx.childView(child) : undefined;
  let route = "进程内（Worker 直接执行）";
  if (child) route = h("button", { class: "link", onclick: () => ctx.openSession(child) }, `子 Session ${short(child)} ↗`);
  else if (node === "@user") route = "用户本人（ask_user）";
  else if (node) route = h("span", {}, l.target ? `${l.target} · ` : "", h("code", {}, short(node)));
  const childRun = cv?.payloads("run_requested")[0];
  const childEnd = childRun && cv.runEnd(childRun.run_id);
  const at = (kind, pred) => {
    const e = (kind === "child" ? (cv?.events ?? []) : v.events).find((x) => pred(x));
    return e ? Date.parse(e.time) : 0;
  };
  const t0 = st.askedAt;
  const resultBlocks = (st.blocks ?? []).filter((b) => !b.text);
  return shell(
    `${l.icon} ${l.name}`,
    l.target ? `@${l.target}` : "",
    ctx,
    section(
      "概要",
      null,
      overview([
        ["状态", pill(state)],
        ["能力", h("code", {}, st.capability)],
        ["所属 Attempt", h("span", { class: "chip mono" }, `A${st.attempt ?? 0}`)],
        ["路由到", route],
        [
          "执行",
          st.executions > 1 ? h("span", { class: "warn" }, `${st.executions} 次（重新投递或接管后重新执行）`) : `${st.executions || 0} 次`,
        ],
        st.background && [
          "后台",
          `${{ result: "设备结果已送达", timeout: "已到期，未执行", cancelled: "已取消" }[st.bgSettled] ?? "等待设备上线"} · 截止 ${clock(st.bgDeadline)}`,
        ],
        st.approval && [
          "审批",
          `${{ pending: "等待中", approved: "已批准", denied: "已拒绝" }[st.approval]}${st.summary ? ` · ${st.summary}` : ""}`,
        ],
        ["调用 ID", h("code", { title: callId }, short(callId))],
      ]),
    ),
    section(
      "时间线",
      h("button", { class: "link", onclick: () => ctx.onTimeline(st.runId) }, "在控制台中查看"),
      steps([
        ["模型发起调用", t0, t0],
        st.approvalAt && ["请求审批", st.approvalAt, t0, "warn"],
        st.decidedAt && [st.approval === "denied" ? "已拒绝" : "已批准", st.decidedAt, t0],
        st.startedAt && ["派发", st.startedAt, t0],
        childRun && ["子 Run 开始", at("child", (e) => e.attempt_started?.run_id === childRun.run_id), t0],
        childEnd && [
          `子 Run ${{ run_completed: "完成", run_failed: "失败", run_interrupted: "中断" }[childEnd.kind]}`,
          at("child", (e) => payloadOf(e)[1].run_id === childRun.run_id && payloadOf(e)[0] === childEnd.kind),
          t0,
        ],
        st.background && ["转入后台（Run 继续）", st.bgAt, t0, "warn"],
        st.background && st.bgSettled && [
          { result: "设备结果送达", timeout: "到期", cancelled: "取消" }[st.bgSettled],
          st.bgSettledAt,
          t0,
          st.bgSettled === "result" && !st.error ? "ok" : "",
        ],
        st.doneAt && !st.background && [st.error ? "结果（错误）" : "结果写回", st.doneAt, t0, st.error ? "bad" : "ok"],
      ]),
    ),
    section("参数", null, jsonView(st.args ?? "{}")),
    st.done
      ? section(
          st.error ? "错误" : "结果",
          null,
          jsonView(st.result ?? ""),
          resultBlocks.length ? jsonView(resultBlocks, { open: 3 }) : null,
        )
      : null,
  );
}

function modelDetail(seq, ctx) {
  const v = ctx.view;
  const e = v.events.find((x) => String(x.seq) === String(seq));
  const p = e?.assistant_message;
  if (!p) return shell("模型调用", "", ctx, h("div", { class: "drawer-empty" }, "日志中没有这条消息"));
  const u = p.usage ?? {};
  const text = textOf(p.content);
  return shell(
    "🧠 模型调用",
    p.model ?? "",
    ctx,
    section(
      "概要",
      h("button", { class: "link", onclick: () => ctx.onTimeline(p.run_id) }, "在控制台中查看"),
      overview([
        ["模型", h("code", {}, p.model ?? "?")],
        ["Run", h("code", { title: p.run_id }, short(p.run_id))],
        ["所属 Attempt", h("span", { class: "chip mono" }, `A${p.attempt ?? 0}`)],
        ["首字", dur(p.first_token_ms) || "—"],
        ["耗时", dur(p.duration_ms) || "—"],
        ["结束原因", p.finish_reason || "—"],
        ["事件", `#${e.seq} · ${clock(e.time)}`],
      ]),
    ),
    section(
      "Token",
      null,
      h(
        "div",
        { class: "token-grid" },
        [
          ["输入", u.input_tokens],
          ["其中缓存", u.cached_input_tokens],
          ["输出", u.output_tokens],
          ["其中思考", u.reasoning_tokens],
        ].map(([k, n]) => h("div", {}, h("b", { class: "mono" }, num(n)), h("span", {}, k))),
      ),
      h("div", { class: "muted small" }, modelLine(p)),
    ),
    p.reasoning ? section("思考过程", null, h("div", { class: "code-box prose-box" }, p.reasoning)) : null,
    text ? section("回复", null, h("div", { class: "code-box prose-box" }, text)) : null,
    (p.tool_calls ?? []).length
      ? section(
          "发起的调用",
          null,
          h(
            "div",
            { class: "call-list" },
            p.tool_calls.map((c) => {
              const l = toolLabel(c.capability);
              return h(
                "button",
                { class: "call-row", onclick: () => ctx.onSelect({ kind: "call", callId: c.call_id }) },
                h("span", { class: "call-icon" }, l.icon),
                h("code", {}, l.name),
                l.target ? h("span", { class: "muted" }, "@", l.target) : null,
              );
            }),
          ),
        )
      : null,
  );
}

function cardDetail(cardId, ctx) {
  const v = ctx.view;
  const versions = [];
  for (const e of v.events)
    for (const b of e.tool_result?.content ?? []) if (b.card?.card_id === cardId) versions.push({ card: b.card, e });
  if (!versions.length) return shell("卡片", cardId, ctx, h("div", { class: "drawer-empty" }, "日志中没有这张卡片"));
  const latest = versions.at(-1).card;
  const actions = [];
  for (const e of v.events) {
    const a = cardActionOf(e);
    if (a?.card_id === cardId) actions.push({ a, e });
  }
  return shell(
    "🃏 卡片",
    cardId,
    ctx,
    section(
      "版本",
      null,
      steps(
        versions.map(({ card, e }, i) => [
          `第 ${i + 1} 版 · ${card.title} · ${short(card.call_id)}`,
          Date.parse(e.time),
          undefined,
          i === versions.length - 1 ? "ok" : "",
        ]),
      ),
    ),
    section(
      "已执行的动作",
      null,
      actions.length
        ? steps(
            actions.map(({ a, e }) => [
              `${a.label || a.action}${Object.keys(a.values ?? {}).length ? ` · ${JSON.stringify(a.values)}` : ""}（第 ${versions.findIndex((x) => x.card.call_id === a.call_id) + 1} 版）${e.tool_result ? " · 作为提问的回答" : ""}`,
              Date.parse(e.time),
            ]),
          )
        : h("div", { class: "muted" }, "还没有"),
    ),
    section("最新一版", null, jsonView(latest, { open: 1 })),
  );
}
