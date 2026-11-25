#!/usr/bin/env bash
# make looptest-adapters：对 sdk/python 的参考适配运行外部循环一致性套件（ADR-0062 第 8 条）。
# 需要 uv 与联网安装锁定的依赖，与 make verify 一样不在 make check 中。
#   looptest-adapters.sh            依次对 langgraph、pydantic 运行 yanshi looptest -step-token
#   looptest-adapters.sh restart F P  重启框架 F 在端口 P 上的适配（looptest 的 restart 场景调用）
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
PY_DIR="$ROOT/sdk/python"
RUN_DIR="$ROOT/build/looptest-adapters"
MODEL_ADDR=127.0.0.1:18471
PY="$PY_DIR/.venv/bin/python"
mkdir -p "$RUN_DIR"

start() {
	local fw=$1 port=$2
	# 只把 Python 进程放到后台（不是 "cd && python" 整个列表），记下的 PID 才是它本身，restart 才停得掉。
	cd "$PY_DIR"
	"$PY" -m yanshi_loop --framework "$fw" --port "$port" --model-base-url "http://$MODEL_ADDR/v1" >>"$RUN_DIR/$fw.log" 2>&1 &
	echo $! >"$RUN_DIR/$fw.pid"
	disown
	cd - >/dev/null
}

stop() {
	local fw=$1
	if [[ -f "$RUN_DIR/$fw.pid" ]]; then
		kill "$(cat "$RUN_DIR/$fw.pid")" 2>/dev/null || true
		rm -f "$RUN_DIR/$fw.pid"
	fi
}

# 等端口可以连接（GET 得到 405 也算）；down 时等它断开。
wait_port() {
	local port=$1 want=${2:-up}
	for _ in $(seq 1 100); do
		if curl -s -o /dev/null "http://127.0.0.1:$port/"; then
			[[ $want == up ]] && return 0
		else
			[[ $want == down ]] && return 0
		fi
		sleep 0.1
	done
	echo "port $port did not come $want" >&2
	return 1
}

if [[ ${1:-} == restart ]]; then
	stop "$2"
	wait_port "$3" down
	start "$2" "$3"
	wait_port "$3" up
	exit 0
fi

(cd "$PY_DIR" && uv sync --frozen --quiet)
(cd "$ROOT" && go build -o bin/yanshi ./cmd/yanshi)

status=0
port=18481
for fw in langgraph pydantic; do
	: >"$RUN_DIR/$fw.log"
	start "$fw" "$port"
	trap 'stop langgraph; stop pydantic' EXIT
	echo "== $fw (port $port)"
	if ! "$ROOT/bin/yanshi" looptest -url "http://127.0.0.1:$port" -model-addr "$MODEL_ADDR" -step-token -wait 30s \
		-restart "bash $PY_DIR/scripts/looptest-adapters.sh restart $fw $port"; then
		status=1
		echo "-- $fw log:" && tail -n 40 "$RUN_DIR/$fw.log"
	fi
	stop "$fw"
	port=$((port + 1))
done
exit $status
