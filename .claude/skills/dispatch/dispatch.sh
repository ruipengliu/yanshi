#!/usr/bin/env bash
# 派发一张任务卡给外部 agent，结果落到 <outdir>，调用方只读 report.md。
#
#   dispatch.sh <codex|gemini> <ro|rw> <workdir> <brief.md> <outdir>
#   dispatch.sh resume <outdir> <followup.md>        # 同一会话返工/追问
#
# outdir 内：report.md（agent 的最后一条消息）、meta（agent/mode/workdir/session）、
# raw.log（原始输出，仅在失败或报告可疑时查看）。
#
# 环境变量（可选）：
#   DISPATCH_CODEX_MODEL   默认 gpt-6.2-sol      DISPATCH_CODEX_EFFORT  默认 high
#   DISPATCH_GEMINI_MODEL  默认 gemini-4-neon-high
#   DISPATCH_ALLOW         gemini 额外放行的规则，逗号分隔，如 'command(go test),command(make lint)'
#   DISPATCH_TIMEOUT       秒，默认 3600
set -euo pipefail

CODEX_MODEL=${DISPATCH_CODEX_MODEL:-gpt-6.2-sol}
CODEX_EFFORT=${DISPATCH_CODEX_EFFORT:-high}
GEMINI_MODEL=${DISPATCH_GEMINI_MODEL:-gemini-4-neon-high}
TIMEOUT=${DISPATCH_TIMEOUT:-3600}
# 本机内存约 1–2 GB：同一时间只跑一个外部 agent，后到的排队等锁。
LOCK=${XDG_RUNTIME_DIR:-/tmp}/dispatch-agent.lock

die() { echo "dispatch: $*" >&2; exit 2; }

# agy 无头模式没有终端沙箱（--mode auto 会退回默认模式），
# 不在放行表里的工具调用会被自动拒绝，且常常直接结束本轮、回复为空，
# 所以权限只能用会话级 --settings 显式给出。
gemini_settings() { # $1=ro|rw $2=workdir
  local ro='"read_file(*)","command(pwd)","command(ls)","command(cat)","command(head)","command(tail)","command(wc)","command(grep)","command(rg)","command(echo)","command(true)","command(sort)","command(uniq)","command(cut)","command(tr)","command(diff)","command(stat)","command(tree)","command(git status)","command(git log)","command(git diff)","command(git show)","command(git grep)","command(git blame)"'
  local extra=""
  if [[ -n ${DISPATCH_ALLOW:-} ]]; then
    extra=$(python3 -c 'import json,sys;print(",".join(json.dumps(r.strip()) for r in sys.argv[1].split(",") if r.strip()))' "$DISPATCH_ALLOW")
    extra=",$extra"
  fi
  if [[ $1 == ro ]]; then
    printf '{"permissions":{"allow":[%s%s],"deny":["write_file(*)"]}}' "$ro" "$extra"
  else
    printf '{"permissions":{"allow":[%s,"write_file(%s)","command(git add)"%s],"deny":["write_file(%s/.git/)","command(git push)","command(git commit)","command(git reset)","command(git checkout)","command(rm -rf)"]}}' \
      "$ro" "$2" "$extra" "$2"
  fi
}

# agy 的 JSON 结果 → report.md / session；被拒绝的动作追加到报告末尾，免得空回复被当成"无话可说"。
gemini_collect() { # $1=raw.log $2=outdir
  python3 - "$1" "$2" <<'PY'
import json, sys, pathlib
raw, out = pathlib.Path(sys.argv[1]).read_text(), pathlib.Path(sys.argv[2])
obj = None
for line in reversed(raw.splitlines()):
    line = line.strip()
    if line.startswith("{") and '"conversation_id"' in line:
        obj = json.loads(line); break
if obj is None:
    (out / "report.md").write_text("（agy 没有返回 JSON 结果，见 raw.log）\n"); sys.exit(1)
rep = obj.get("response") or "（空回复）"
denied = obj.get("denied_actions") or []
if denied:
    rep += "\n\n---\n被拒绝的动作（需放行或换方式）：" + ", ".join(d.get("display_name", d.get("action", "?")) for d in denied) + "\n"
(out / "report.md").write_text(rep)
(out / "session").write_text(obj["conversation_id"])
u = obj.get("usage", {})
print(f"status={obj.get('status')} tokens={u.get('total_tokens')} denied={len(denied)}")
PY
}

run_codex() { # $1=mode $2=workdir $3=prompt-file $4=outdir [$5=session]
  local sandbox=read-only; [[ $1 == rw ]] && sandbox=workspace-write
  cd "$2"
  if [[ -z ${5:-} ]]; then
    timeout "$TIMEOUT" codex exec -m "$CODEX_MODEL" -c model_reasoning_effort="\"$CODEX_EFFORT\"" \
      -s "$sandbox" --skip-git-repo-check -o "$4/report.md" - <"$3" >"$4/raw.log" 2>&1 || echo "codex exit=$?（见 raw.log）"
    { grep -m1 -oE 'session id: [0-9a-f-]+' "$4/raw.log" || true; } | awk '{print $3}' >"$4/session"
  else
    # resume 不接受 -s / -C：沙箱经 -c 给出，工作目录靠 cd。
    timeout "$TIMEOUT" codex exec resume "$5" -m "$CODEX_MODEL" -c model_reasoning_effort="\"$CODEX_EFFORT\"" \
      -c sandbox_mode="\"$sandbox\"" --skip-git-repo-check -o "$4/report.md" - <"$3" >>"$4/raw.log" 2>&1 || echo "codex exit=$?（见 raw.log）"
  fi
  { grep -A1 'tokens used' "$4/raw.log" || true; } | tail -1 | sed 's/^/tokens=/'
}

run_gemini() { # $1=mode $2=workdir $3=prompt-file $4=outdir [$5=session]
  local conv=(); [[ -n ${5:-} ]] && conv=(--conversation "$5")
  local editmode=(); [[ $1 == rw ]] && editmode=(--mode accept-edits)
  cd "$2"
  # 组合命令（|、||、&&）的每一段都要在放行表里，否则整轮被中止；先告诉它边界，让它自己避开。
  local pre="[运行约束] shell 只能用：ls cat head tail wc grep rg echo sort uniq cut tr diff stat tree，以及 git 的 status/log/diff/show/grep/blame"
  [[ $1 == rw ]] && pre+="/add；文件只能写在 $2 内"
  [[ -n ${DISPATCH_ALLOW:-} ]] && pre+="；另放行：$DISPATCH_ALLOW"
  pre+="。读文件、搜索优先用内置工具。其他命令会被拒绝并中止本轮，需要时在报告里说明。"
  # -p 必须写成 -p=...，否则它会把下一个参数当成提示词。
  timeout "$TIMEOUT" agy --model "$GEMINI_MODEL" "${editmode[@]}" "${conv[@]}" \
    --settings "$(gemini_settings "$1" "$2")" --output-format json -p="$pre

$(cat "$3")" >>"$4/raw.log" 2>&1 || true
  gemini_collect "$4/raw.log" "$4"
}

if [[ ${1:-} == resume ]]; then
  [[ $# -eq 3 ]] || die "用法: dispatch.sh resume <outdir> <followup.md>"
  out=$(realpath "$2"); prompt=$(realpath "$3")
  # shellcheck disable=SC1091
  source "$out/meta"
  session=$(cat "$out/session" 2>/dev/null) || die "$out 没有 session，无法续聊"
  exec 9>"$LOCK"; flock 9
  "run_$agent" "$mode" "$workdir" "$prompt" "$out" "$session"
else
  [[ $# -eq 5 ]] || die "用法: dispatch.sh <codex|gemini> <ro|rw> <workdir> <brief.md> <outdir>"
  agent=$1 mode=$2
  [[ $agent == codex || $agent == gemini ]] || die "agent 只能是 codex 或 gemini"
  [[ $mode == ro || $mode == rw ]] || die "mode 只能是 ro 或 rw"
  workdir=$(realpath "$3"); prompt=$(realpath "$4")
  mkdir -p "$5"; out=$(realpath "$5")
  printf 'agent=%s\nmode=%s\nworkdir=%s\n' "$agent" "$mode" "$workdir" >"$out/meta"
  exec 9>"$LOCK"; flock 9
  "run_$agent" "$mode" "$workdir" "$prompt" "$out"
fi
echo "report: $out/report.md"
