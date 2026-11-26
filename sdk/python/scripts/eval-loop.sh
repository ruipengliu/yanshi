#!/usr/bin/env bash
# make eval-loop：外部循环的冒烟评测。启动一个参考适配（默认 pydantic），以 yanshi eval -loop 驱动 evals/loop 的用例；
# 适配经评测的内核模型端点（127.0.0.1:18472）调用真实模型，消耗真实 token（需 ARK_API_KEY），不在 make check 中。
#   eval-loop.sh [langgraph|pydantic] [yanshi eval 的其他参数…]
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
PY_DIR="$ROOT/sdk/python"
RUN_DIR="$ROOT/build/eval-loop"
FW=${1:-pydantic}
shift || true
PORT=18491
mkdir -p "$RUN_DIR"

(cd "$PY_DIR" && uv sync --frozen --quiet)
cd "$PY_DIR"
.venv/bin/python -m yanshi_loop --framework "$FW" --port "$PORT" --model-base-url http://127.0.0.1:18472/v1/loop \
	>"$RUN_DIR/$FW.log" 2>&1 &
pid=$!
disown
trap 'kill $pid 2>/dev/null || true' EXIT
cd "$ROOT"
for _ in $(seq 1 100); do
	curl -s -o /dev/null "http://127.0.0.1:$PORT/" && break
	sleep 0.1
done
./bin/yanshi eval -suite evals/loop -agents evals/loop/agents -loop "ref=http://127.0.0.1:$PORT" "$@"
