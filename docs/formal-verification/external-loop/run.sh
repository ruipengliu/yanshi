#!/usr/bin/env bash
# 外部循环：一步的提交、重试与接管（ADR-0062 第 2–5 条）。外部循环尚未实现，这里只验证设计；
# 实现时补上由模型调度驱动的 Go 场景（验证方案第 3 节）。
set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$DIR"

SAMPLES="${SAMPLES:-10000}"
STEPS="${STEPS:-50}"
SEED="${SEED:-0x100b}"

ms_now() {
  node -e 'process.stdout.write(String(Date.now()))'
}

run_step() {
  local name="$1"
  shift
  local t0
  echo "==> $name"
  t0="$(ms_now)"
  "$@"
  echo "    [PASS] $name ($(($(ms_now) - t0))ms)"
}

# 变体须违反指定的不变量：只看退出码会把类型错误等失败误当作“抓到了”。
expect_violation() {
  local name="$1"
  shift
  local t0 out rc
  echo "==> $name (expecting violation)"
  t0="$(ms_now)"
  set +e
  out="$("$@" 2>&1)"
  rc=$?
  set -e
  if [ "$rc" -eq 0 ] || ! grep -q '\[violation\]' <<<"$out"; then
    echo "$out" | tail -20 >&2
    echo "    [FAIL] $name was not caught (exit $rc)" >&2
    exit 1
  fi
  echo "    [PASS] $name caught ($(($(ms_now) - t0))ms)"
}

sim() {
  local main="$1" inv="$2"
  pnpm exec quint run --main="$main" --seed="$SEED" --max-samples="$SAMPLES" --max-steps="$STEPS" --invariant="$inv" external_loop.qnt
}

total_t0="$(ms_now)"

run_step "1. Quint typecheck" \
  pnpm exec quint typecheck external_loop.qnt

run_step "2. Scenario tests" \
  pnpm exec quint test --main=scenarios external_loop.qnt

run_step "3. Random simulation correct ($SAMPLES samples x $STEPS steps)" \
  sim correct allInvariants

# 见证证明模拟走到了重复请求、迟到回答被放弃、不合法后重试、审核等待后恢复、拦截、并行调用、带尾巴的压缩与第二个 Run。
for w in witnessNoRepeatedStep witnessNoStaleRefused witnessNoRetryAfterInvalid witnessNoResumeAfterSuspend \
  witnessNoBlocked witnessNoAfterParallel witnessNoCompactedWithTail witnessNoSecondRunStep; do
  expect_violation "4. Reachability $w" sim correct "$w"
done

# 不做 Apalache 有界检查：日志是列表，投影靠 foldl 与 select 逐步重算，Mac mini 上 400 秒只检查到第 4 步
# （2026-11-24 实测，6 步与 8 步都超时），而走完一次接管加迟到回答至少要 8 步。深交错靠第 3 步与下面的变体。

# 每个变体违反 ADR-0062 的一处，随机模拟须找到反例。
for spec in \
  no_fencing:invFenced \
  worker_memory_state:invStateChain \
  cross_run_state:invStateChain \
  execute_unlogged:invActsOnlyLogged \
  skip_pending:invPairing \
  log_invalid:invOnlyValidLogged \
  block_keeps_step:invBlockedEnds \
  builtin_compaction:invLatestStepRaw; do
  expect_violation "5. Mutant ${spec%%:*} (${spec##*:})" sim "${spec%%:*}" "${spec##*:}"
done

echo ""
echo "All external loop checks passed in $(($(ms_now) - total_t0))ms."
