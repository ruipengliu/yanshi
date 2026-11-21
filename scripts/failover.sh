#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
# 显式 -p/-f，忽略 dev 注入的共享 COMPOSE_FILE/COMPOSE_PROJECT_NAME。
compose() { docker compose -p yanshi-failover -f deploy/failover/compose.yaml "$@"; }
case "${1:-run}" in
  run|capacity|move|down) ;;
  *) echo 'usage: scripts/failover.sh [run|capacity|move|down]' >&2; exit 2 ;;
esac
# 一个项目只允许一个驱动；跨工作树也不能并发重置固定端口。
lock="${TMPDIR:-/tmp}/yanshi-failover.lock"
mkdir "$lock" 2>/dev/null || { echo 'failover project is locked' >&2; exit 1; }
trap 'rmdir "$lock"' EXIT
if [[ "${1:-run}" == down ]]; then
  compose --profile move down -v --remove-orphans
  exit
fi
export YANSHI_TEST_PG='postgres://yanshi:failover-test@127.0.0.1:55432/yanshi?sslmode=disable&connect_timeout=2'
export YANSHI_FAILOVER_PRIMARY='postgres://yanshi:failover-test@127.0.0.1:55430/yanshi?sslmode=disable&connect_timeout=2'
export YANSHI_FAILOVER_STANDBY='postgres://yanshi:failover-test@127.0.0.1:55431/yanshi?sslmode=disable&connect_timeout=2'
export YANSHI_DAY_BIN="$PWD/bin/xinli-day"
mkdir -p eval-results/failover
if [[ "${1:-run}" == move ]]; then
  # 整片搬迁（部署 §10.5）：每个数据量一套全新集群；主库起初没有备库，新主机由演练在线建立。
  export YANSHI_FAILOVER_NEWHOST='postgres://yanshi:failover-test@127.0.0.1:55433/yanshi?sslmode=disable&connect_timeout=2'
  for gb in ${YANSHI_MOVE_GB:-1 10}; do
    rm -f "eval-results/failover/shard-move-${gb}g.json"
    compose --profile move down -v --remove-orphans
    compose up -d --wait primary healthy
    YANSHI_TEST_FAILOVER=1 YANSHI_MOVE_BALLAST_GB="$gb" go test ./internal/day -run '^TestShardMoveKernel$' -count=1 -timeout 60m -v
  done
elif [[ "${1:-run}" == capacity ]]; then
  rm -f eval-results/failover/capacity-1.json eval-results/failover/capacity-2.json
  compose --profile move down -v --remove-orphans
  compose up -d --wait
  YANSHI_TEST_CAPACITY=1 go test ./internal/day -run '^TestShardCapacity$' -count=1 -timeout 20m -v
else
  rm -f eval-results/failover/kernel.json eval-results/failover/persona.json
  for kind in Kernel Persona; do
    compose --profile move down -v --remove-orphans
    compose up -d --wait
    YANSHI_TEST_FAILOVER=1 go test -race ./internal/day -run "^TestFailover${kind}$" -count=1 -timeout 15m -v
  done
fi
# 保留最后一轮现场，由显式 down 清理；不会自动重启已隔离旧主。
