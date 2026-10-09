#!/usr/bin/env bash
# 扩容压测编排（docs/design/m2-scale-test.md）：全新数据库 + P 个 serve 进程（每个 W 个 Worker，开启鉴权），
# 运行 yanshi bench，并抓取各进程的 /metrics。
#
#   P=2 W=16 S=64 DURATION=30s scripts/bench.sh
#   FAULT_AFTER=10 ...   压测开始 10 秒后 kill -9 最后一个进程（故障实验）
#   API=1 ...            只有第一个进程对外提供 API（其余 -workers 但不接客户端）
#   POOL=40 ...          每个进程的数据库连接池上限（默认 pgx：max(4, CPU 数)）
#   S=0 ...              空闲实验：不施加负载，只观察 DURATION 内空闲 Worker 的数据库开销
#   QUOTA=1 ...          为业务线配置（不会触发的）配额，测量配额检查的开销
#
# 结果写入 bench-results/<时间戳>/：bench.json、各进程 metrics 与日志。
set -euo pipefail
cd "$(dirname "$0")/.."

P=${P:-1} W=${W:-16} S=${S:-64} DURATION=${DURATION:-30s} AGENT=${AGENT:-bench}
FAULT_AFTER=${FAULT_AFTER:-} API=${API:-$P}
TS=$(date +%Y%m%d-%H%M%S)
OUT=bench-results/$TS
DB=bench_${TS//-/_}
mkdir -p "$OUT"

make build >/dev/null
make deps-up >/dev/null 2>&1
docker compose exec -T postgres psql -q -U yanshi -c "CREATE DATABASE $DB" >/dev/null
docker compose exec -T postgres psql -q -U yanshi -d "$DB" -c "CREATE EXTENSION IF NOT EXISTS pg_stat_statements" >/dev/null
DSN="postgres://yanshi:yanshi@127.0.0.1:54329/$DB?sslmode=disable${POOL:+&pool_max_conns=$POOL}"

KEYDIR=$(mktemp -d)
(cd "$KEYDIR" && "$OLDPWD/bin/yanshi" keygen -business-line bench >/dev/null)
if [ -n "${QUOTA:-}" ]; then
  # 配额足够大、不会触发，只测量每次模型调用前检查配额的开销（docs/design/m4-quota-usage.md）。
  printf 'quota:\n  monthly: 1000000\n  end_user_daily: 1000\n' >> "$KEYDIR/businesslines/bench.yaml"
fi
export YANSHI_PEER_TOKEN=bench-peer-token

pids=() servers=()
cleanup() {
  for pid in "${pids[@]}"; do kill "$pid" 2>/dev/null || true; done
  wait 2>/dev/null || true
  rm -rf "$KEYDIR"
  docker compose exec -T postgres psql -q -U yanshi -c "DROP DATABASE IF EXISTS $DB WITH (FORCE)" >/dev/null 2>&1 || true
}
trap cleanup EXIT

for i in $(seq 0 $((P - 1))); do
  ./bin/yanshi serve -storage postgres -pg-dsn "$DSN" -auth jwt -businesslines "$KEYDIR/businesslines" \
    -addr 127.0.0.1:$((18300 + i)) -peer-addr 127.0.0.1:$((18400 + i)) -workers "$W" >"$OUT/serve-$i.log" 2>&1 &
  pids+=($!)
  if [ "$i" -lt "$API" ]; then servers+=("http://127.0.0.1:$((18300 + i))"); fi
done
for i in $(seq 0 $((P - 1))); do
  until curl -sf "127.0.0.1:$((18300 + i))/healthz" >/dev/null; do sleep 0.2; done
done

if [ -n "$FAULT_AFTER" ]; then
  victim=${pids[$((P - 1))]}
  (sleep "$FAULT_AFTER" && kill -9 "$victim" && echo "[fault] killed serve-$((P - 1)) (pid $victim) at ${FAULT_AFTER}s" >&2) &
fi

echo "P=$P W=$W S=$S duration=$DURATION agent=$AGENT api=${#servers[@]} → $OUT" >&2
if [ "$S" = 0 ]; then
  # 空闲实验（E3）：不施加负载，只观察空闲 Worker 的开销。
  docker compose exec -T postgres psql -q -U yanshi -d "$DB" -c "SELECT pg_stat_statements_reset()" >/dev/null
  sleep "${DURATION%s}"
  echo '{"RunsPerSecond":0,"Runs":{},"LatencyMs":null,"TTFTMs":null,"Errors":null}' >"$OUT/bench.json"
else
./bin/yanshi bench -servers "$(IFS=,; echo "${servers[*]}")" -sessions "$S" -duration "$DURATION" \
  -agent "$AGENT" -key "$KEYDIR/bench.key" -out "$OUT/bench.json" >/dev/null
fi

# 每类查询的次数与总耗时（只统计本次压测的数据库）。
docker compose exec -T postgres psql -U yanshi -d "$DB" -P pager=off -c "
  SELECT calls, round(total_exec_time::numeric) AS total_ms, round(mean_exec_time::numeric, 3) AS mean_ms,
         left(regexp_replace(query, '\s+', ' ', 'g'), 110) AS query
  FROM pg_stat_statements WHERE dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
  ORDER BY calls DESC LIMIT 15" >"$OUT/queries.txt" 2>&1 || true

for i in $(seq 0 $((P - 1))); do
  curl -sf "127.0.0.1:$((18400 + i))/metrics" >"$OUT/metrics-$i.txt" 2>/dev/null || true
done
python3 - "$OUT/bench.json" <<'EOF'
import json, sys
d = json.load(open(sys.argv[1]))
print(f"runs/s={d['RunsPerSecond']:.1f}  runs={d['Runs']}  latency={d['LatencyMs']}  ttft_p50={d['TTFTMs'] and d['TTFTMs']['p50']}  errors={d['Errors']}")
EOF
