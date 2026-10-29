#!/usr/bin/env bash
# 成本测试显式启用，避免 make check 每次重建全年工作集。
set -euo pipefail
cd "$(dirname "$0")/.."

: "${YANSHI_TEST_PG:?请配置测试建库入口 YANSHI_TEST_PG（须有 CREATEDB 权限）}"
export PERSONA_BENCH=1
export PERSONA_BENCH_N="${PERSONA_BENCH_N:-200}"

printf 'Persona 大工作集成本（A14）\n\n'
printf '运行时间：'
date -u '+%Y-%m-%dT%H:%M:%SZ'
printf '源码 HEAD：'
# dev 同步到 Mac mini 的源码没有 .git，源码指纹由 dev 的输出给出。
git rev-parse HEAD 2>/dev/null || echo '（无 git：见 dev 输出的源码指纹）'
if git rev-parse --git-dir >/dev/null 2>&1; then
  printf '工作区改动：\n'
  git status --short
fi
uname -srm
if [[ "$(uname -s)" == Darwin ]]; then
  sw_vers
  sysctl hw.model hw.memsize hw.ncpu
  printf 'OrbStack 版本：'
  defaults read /Applications/OrbStack.app/Contents/Info CFBundleShortVersionString || true
fi
if command -v docker >/dev/null 2>&1; then
  docker version --format 'Docker client={{.Client.Version}} server={{.Server.Version}}' || true
  docker info --format 'Docker OS={{.OperatingSystem}} CPUs={{.NCPU}} memory={{.MemTotal}}' || true
fi
printf '\nN=%s；每轮重建配方，准备时间不计入样本。\n' "$PERSONA_BENCH_N"
printf 'Go 堆设置：GOGC=%s GOMEMLIMIT=%s\n' "${GOGC:-默认}" "${GOMEMLIMIT:-默认}"
go -C persona list -m github.com/jackc/pgx/v5
# 不带 race；使用无限测试期限，避免准备成本使 Go 默认十分钟超时截断正式报告。
go -C persona test ./store/costtest -run '^TestLargeWorkingSetCost$' -count=1 -timeout=0 -v
