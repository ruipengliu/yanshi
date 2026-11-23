#!/usr/bin/env bash
# A5 迁槽即换代：ADR-0046“迁槽与游标”的步骤顺序。迁槽工具尚未实现，这里只验证设计；
# 工具实现时补上与真实实现的轨迹对照（同 PoC #1 的第 6 步）。
set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$DIR"

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

total_t0="$(ms_now)"

run_step "1. Quint typecheck" \
  pnpm exec quint typecheck slot_migration.qnt

run_step "2. Scenario tests" \
  pnpm exec quint test --main=scenarios slot_migration.qnt

run_step "3. Random simulation (100000 samples x 40 steps)" \
  pnpm exec quint run --main=correct --seed=0x5a5 --max-samples=100000 --max-steps=40 --invariant=allInvariants slot_migration.qnt

for w in witnessNoSettledRollback witnessNoUndrainedLoss; do
  expect_violation "4. Reachability $w" \
    pnpm exec quint run --main=correct --seed=0x5a5 --max-samples=100000 --max-steps=40 --invariant="$w" slot_migration.qnt
done

# 符号检查的代价随步数约三倍增长（Mac mini：5 步 22 秒、6 步 78 秒），只做浅层穷尽；深层交错靠第 3 步。
# TLC 穷尽全状态空间不可行（15 分钟后状态文件已达 17 GB）。
run_step "5. Apalache bounded check (max-steps=5)" \
  pnpm exec quint verify --main=correct --max-steps=5 --invariant=allInvariants slot_migration.qnt

# 每个变体违反 ADR 的一处顺序，随机模拟须找到反例。
for spec in \
  keep_source_rows:invNoResidue \
  keep_source_copy:invNoResidue \
  drop_source_at_copy:invNoFalseGone \
  no_target_rotation:invConverged \
  rotate_before_switch:invConverged \
  rotate_during_rollout:invConverged \
  drain_before_stop:invDrainKeepsReasons; do
  expect_violation "6. Mutant ${spec%%:*} (${spec##*:})" \
    pnpm exec quint run --main="${spec%%:*}" --seed=0x5a5 --max-samples=100000 --max-steps=40 --invariant="${spec##*:}" slot_migration.qnt
done

echo ""
echo "All A5 slot migration checks passed in $(($(ms_now) - total_t0))ms."
