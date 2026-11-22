#!/usr/bin/env bash
set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$DIR/../../.." && pwd)"
cd "$DIR"

# ms_now 返回毫秒时间戳（兼容 macOS 无 GNU date %N）
ms_now() {
  node -e 'process.stdout.write(String(Date.now()))'
}

run_step() {
  local name="$1"
  shift
  local t0 t1 elapsed
  echo "==> $name"
  t0="$(ms_now)"
  "$@"
  t1="$(ms_now)"
  elapsed=$((t1 - t0))
  echo "    [PASS] $name (${elapsed}ms)"
}

expect_fail() {
  local name="$1"
  shift
  local t0 t1 elapsed
  echo "==> $name (expecting violation)"
  t0="$(ms_now)"
  set +e
  "$@"
  local rc=$?
  set -e
  t1="$(ms_now)"
  elapsed=$((t1 - t0))
  if [ "$rc" -eq 0 ]; then
    echo "    [FAIL] $name unexpectedly passed (exit 0)!" >&2
    exit 1
  fi
  echo "    [PASS] $name caught violation as expected (exit $rc, ${elapsed}ms)"
}

total_t0="$(ms_now)"

run_step "1. Quint typecheck (spec + mutants)" \
  bash -c 'pnpm exec quint typecheck lifecycle_recheck.qnt && pnpm exec quint typecheck lifecycle_recheck_mutant.qnt'

run_step "2. Quint built-in unit tests" \
  pnpm exec quint test lifecycle_recheck.qnt

run_step "3. Quint random simulation (2000 samples x 25 steps)" \
  pnpm exec quint run --max-samples=2000 --max-steps=25 --invariant=allInvariants lifecycle_recheck.qnt

run_step "4. Quint SMT symbolic verification (Apalache, max-steps=12)" \
  pnpm exec quint verify --max-steps=12 --invariant=allInvariants lifecycle_recheck.qnt

expect_fail "5a. Mutation test #1: CheckThenWrite (Apalache SMT)" \
  pnpm exec quint verify --main=mutant_check_then_write --max-steps=10 --invariant=invNoOrdinaryDataAfterComplete lifecycle_recheck_mutant.qnt

expect_fail "5b. Mutation test #2: ErasureSkipsRecheck (Apalache SMT)" \
  pnpm exec quint verify --main=mutant_erasure_skips_recheck --max-steps=10 --invariant=invNoLateMemorySurvived lifecycle_recheck_mutant.qnt

run_step "6a. Go real implementation trace export (-race)" \
  bash -c "cd '$ROOT' && YANSHI_TRACE_OUT='$DIR/generated/trace_replay_test.qnt' go test -race -v ./internal/sim -run '^TestLifecycleRecheckTraceValidation\$'"

run_step "6b. Quint trace replay validation" \
  pnpm exec quint test generated/trace_replay_test.qnt

total_t1="$(ms_now)"
echo ""
echo "All PoC #1 verification gates passed in $((total_t1 - total_t0))ms."
