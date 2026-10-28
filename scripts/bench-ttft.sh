#!/usr/bin/env bash
# Q81：同机、同一固定问答到达计划；只验收并发瓶颈且 RPS 不受限的负载。
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ ${YANSHI_BENCH_TTFT:-} != 1 ]]; then
  echo '请显式设置 YANSHI_BENCH_TTFT=1；本压测不进 make check。' >&2
  exit 1
fi
C=${C:-32} O=${O:-8} B=${B:-4} CLIENTS=${CLIENTS:-64} WORKERS=${WORKERS:-64}
RATE=${RATE:-16} TTFT=${TTFT:-50ms} LATENCY=${LATENCY:-1s}
WARMUP=${WARMUP:-20s} DURATION=${DURATION:-60s} RUN_TIMEOUT=${RUN_TIMEOUT:-30s}
PORT=${PORT:-18580} OUT=${OUT:-bench-results/ttft-$(date +%Y%m%d-%H%M%S)}
BIN=${BIN:-./bin/yanshi}
# 全部 Session/SSE 在采样前准备，默认 1280 条；macOS 的默认软限制可能只有 256。
ulimit -n 8192 || true
mkdir -p "$OUT"
bench_tmp=$(mktemp -d)
pid=''
cleanup() {
  if [[ -n "$pid" ]]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi
  rm -rf "$bench_tmp"
}
trap cleanup EXIT
python3 - "$C" "$O" "$B" "$CLIENTS" "$WORKERS" "$RATE" <<'PY'
import sys
c, o, b, clients, workers = map(int, sys.argv[1:6])
rate = float(sys.argv[6])
if not (0 < o < c and 0 <= b < c and clients >= 2*c and workers >= c and rate > 0):
    sys.exit('要求 0<O<C、0<=B<C、CLIENTS>=2*C、WORKERS>=C、RATE>0')
PY
mkdir -p "$bench_tmp/agents" "$bench_tmp/oneshots"
cat > "$bench_tmp/agents/bench.yaml" <<YAML
name: bench
version: "1"
model: bench/latency=$LATENCY,ttft=$TTFT,turns=0
instructions: ""
max_turns: 2
YAML
printf '结论范围：只覆盖并发瓶颈、RPS 不受限的负载；model-rps=0。\n'
for mode in baseline mixed control; do
  shot_max=$O
  if [[ "$mode" == control ]]; then
    shot_max=$CLIENTS
  fi
  # 独立临时目录，不向全局 oneshots/ 添加压测声明；纯文本输出不触发 Schema 修复。
  cat > "$bench_tmp/oneshots/bench-brief.yaml" <<YAML
name: bench-brief
version: "1"
model: bench/latency=$LATENCY,ttft=$TTFT,turns=0
instructions: "仅用于并发压测，返回固定片段。"
input: {max_chars: 8000}
max_output_tokens: 300
concurrency: $shot_max
work_class: interactive
YAML
  server_args=(serve -addr "127.0.0.1:$PORT" -roles gateway,worker
    -storage memory -auth none -sandbox none -blob fs -blob-dir "$bench_tmp/artifacts"
    -agents "$bench_tmp/agents" -oneshots "$bench_tmp/oneshots" -mcp "$bench_tmp/mcp"
    -businesslines "$bench_tmp/businesslines" -pricing "$bench_tmp/pricing.yaml"
    -workers "$WORKERS" -interactive-workers 0
    -model-concurrency "$C" -model-min-background "$B" -model-rps 0
    -oneshot-concurrency "$shot_max" -oneshot-wait 200ms -oneshot-deadline "$RUN_TIMEOUT")
  client_args=(bench -servers "http://127.0.0.1:$PORT" -key '' -agent bench
    -rate "$RATE" -warmup "$WARMUP" -duration "$DURATION" -run-timeout "$RUN_TIMEOUT")
  if [[ "$mode" == control ]]; then
    server_args+=(-bench-oneshot-unreserved)
  fi
  if [[ "$mode" != baseline ]]; then
    client_args+=(-oneshot bench-brief -oneshot-concurrency "$CLIENTS")
  fi
  # 参数原样保存，客户端报告不猜测服务端配置。
  printf '%q ' "$BIN" "${server_args[@]}" > "$OUT/$mode-command.txt"
  printf '\n' >> "$OUT/$mode-command.txt"
  printf '%q ' "$BIN" "${client_args[@]}" >> "$OUT/$mode-command.txt"
  printf '\n' >> "$OUT/$mode-command.txt"
  cp "$bench_tmp/agents/bench.yaml" "$OUT/$mode-agent.yaml"
  cp "$bench_tmp/oneshots/bench-brief.yaml" "$OUT/$mode-oneshot.yaml"
  if curl --max-time 1 -sf "http://127.0.0.1:$PORT/readyz" >/dev/null; then
    echo "端口 $PORT 已有服务，改用空闲 PORT 后重跑。" >&2
    exit 1
  fi
  "$BIN" "${server_args[@]}" > "$OUT/$mode-server.log" 2>&1 &
  pid=$!
  ready=0
  for ((i=0; i<150; i++)); do
    if ! kill -0 "$pid" 2>/dev/null; then echo "$mode 服务启动失败，见 $OUT/$mode-server.log" >&2; exit 1; fi
    if curl --max-time 1 -sf "http://127.0.0.1:$PORT/readyz" >/dev/null; then ready=1; break; fi
    sleep 0.2
  done
  if [[ "$ready" != 1 ]]; then echo "$mode 服务未就绪" >&2; exit 1; fi
  "$BIN" "${client_args[@]}" -out "$OUT/$mode.json" > "$OUT/$mode-stdout.json"
  kill "$pid"
  wait "$pid" || true
  pid=''
done
python3 - "$OUT" <<'PY' | tee "$OUT/comparison.txt"
import json, math, pathlib, sys
root = pathlib.Path(sys.argv[1])
results = {name: json.loads((root / (name + '.json')).read_text()) for name in ('baseline', 'mixed', 'control')}
print('结论范围：并发瓶颈、RPS 不受限；不代表真实模型、共享 RPS 耗尽或多进程全局容量。')
print('| 配置 | 问答数 | 成功率 | 首字 p50/p95/p99 ms | 端到端 p50/p95/p99 ms | 单次成功/s | 单次 429 比例 |')
print('|---|---:|---:|---|---:|---:|---:|')
valid = True
counts = []
for name, r in results.items():
    count = sum(r['Runs'].values())
    counts.append(count)
    ttft, latency, shots = r['TTFTMs'] or {}, r['LatencyMs'] or {}, r['Oneshots']
    values = '/'.join(f'{ttft.get(q, math.nan):.2f}' for q in ('p50', 'p95', 'p99'))
    ends = '/'.join(f'{latency.get(q, math.nan):.2f}' for q in ('p50', 'p95', 'p99'))
    print(f"| {name} | {count} | {r['SuccessRate']:.2%} | {values} | {ends} | {shots['SuccessPerSecond']:.2f} | {shots['Rejected429Ratio']:.2%} |")
    # 丢增量、丢到达、客户端发起明显落后均不能靠幸存请求的 p99 验收。
    valid &= count > 0 and r['MissingTTFT'] == 0 and r['TTFTSamples'] > 0
    valid &= (r['ArrivalLagMs'] or {}).get('p99', math.inf) <= 20
    if name != 'baseline':
        valid &= shots['SuccessPerSecond'] > 0
        valid &= all(status in ('completed', '429') for status in (shots['Outcomes'] or {}))
valid &= len(set(counts)) == 1
base, mixed, control = (results[n] for n in ('baseline', 'mixed', 'control'))
b = (base['TTFTMs'] or {}).get('p99', math.nan)
m = (mixed['TTFTMs'] or {}).get('p99', math.nan)
c = (control['TTFTMs'] or {}).get('p99', math.nan)
allowance = max(0.10*b, 20)
# 基线须全成功；否则“成功率不降”可能掩盖环境故障。
passed = valid and base['SuccessRate'] == 1 and mixed['SuccessRate'] >= base['SuccessRate']
passed &= math.isfinite(b) and m-b <= allowance
# “明显退化”固定为超过 mixed 的同一容差；没触发瓶颈的对照必须失败。
control_valid = math.isfinite(c) and c-b > allowance
passed &= control_valid
print(f'mixed−baseline={m-b:.2f}ms，容差={allowance:.2f}ms；control−baseline={c-b:.2f}ms。')
if not control_valid:
    print('对照未测出明显退化：实验未触发所需瓶颈，不能据此验收。')
print('PASS' if passed else 'FAIL（检查失败分类、首字缺失、到达延误与服务端日志）')
sys.exit(0 if passed else 1)
PY
