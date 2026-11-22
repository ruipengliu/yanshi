// -*- mode: Bluespec; -*-
/**
 * yanshi PoC #1 规约变异测试 (Mutation Testing，防空洞证明)
 *
 * 故意注入两类真实协议缺陷，验证 lifecycle_recheck 的安全不变量必然失败并输出反例：
 *   1. mutant_check_then_write: 将普通 SessionData 写入改为“先查删除记录，后写入且不做 Recheck”。
 *   2. mutant_erasure_skips_recheck: 误以为 KeepOnRetention 在 erasure 时豁免 Recheck。
 */

module mutant_check_then_write {
  pure val ORD_IDS: Set[str] = Set("d1")
  pure val REASONS: Set[str] = Set("end_user")

  var tombstone: str
  var completed: bool
  var hasLog: bool
  var hasIndex: bool
  var data: Set[str]
  var janitorStage: str
  // "idle" -> "checked_ok" (先查未删除) -> "done" (写入落盘，无后查)
  var ordPC: str -> str

  val writersSettled: bool =
    ORD_IDS.forall(id => ordPC.get(id) != "checked_ok")

  action init = all {
    tombstone' = "none",
    completed' = false,
    hasLog' = true,
    hasIndex' = true,
    data' = Set(),
    janitorStage' = "idle",
    ordPC' = ORD_IDS.mapBy(_ => "idle"),
  }

  action markDeleted(reason: str): bool = all {
    REASONS.contains(reason),
    tombstone == "none",
    tombstone' = reason,
    completed' = completed,
    hasLog' = hasLog,
    hasIndex' = hasIndex,
    data' = data,
    janitorStage' = janitorStage,
    ordPC' = ordPC,
  }

  // 变异缺陷 1a：先检查未删除，认为安全
  action ordCheckBeforeWrite(id: str): bool = all {
    ORD_IDS.contains(id),
    ordPC.get(id) == "idle",
    tombstone == "none",
    ordPC' = ordPC.set(id, "checked_ok"),
    tombstone' = tombstone,
    completed' = completed,
    hasLog' = hasLog,
    hasIndex' = hasIndex,
    data' = data,
    janitorStage' = janitorStage,
  }

  // 变异缺陷 1b：随后写入落盘，没有写后复查 (Recheck)
  action ordWriteWithoutRecheck(id: str): bool = all {
    ORD_IDS.contains(id),
    ordPC.get(id) == "checked_ok",
    data' = data.union(Set(id)),
    ordPC' = ordPC.set(id, "done"),
    tombstone' = tombstone,
    completed' = completed,
    hasLog' = hasLog,
    hasIndex' = hasIndex,
    janitorStage' = janitorStage,
  }

  action janitorClaim: bool = all {
    tombstone != "none",
    not(completed),
    janitorStage == "idle",
    janitorStage' = "interrupt",
    tombstone' = tombstone,
    completed' = completed,
    hasLog' = hasLog,
    hasIndex' = hasIndex,
    data' = data,
    ordPC' = ordPC,
  }

  action janitorInterrupt: bool = all {
    janitorStage == "interrupt",
    janitorStage' = "sweep1",
    tombstone' = tombstone,
    completed' = completed,
    hasLog' = hasLog,
    hasIndex' = hasIndex,
    data' = data,
    ordPC' = ordPC,
  }

  action janitorSweep1: bool = all {
    janitorStage == "sweep1",
    janitorStage' = "delete_log",
    data' = Set(),
    tombstone' = tombstone,
    completed' = completed,
    hasLog' = hasLog,
    hasIndex' = hasIndex,
    ordPC' = ordPC,
  }

  action janitorDeleteLog: bool = all {
    janitorStage == "delete_log",
    janitorStage' = "sweep2",
    hasLog' = false,
    tombstone' = tombstone,
    completed' = completed,
    hasIndex' = hasIndex,
    data' = data,
    ordPC' = ordPC,
  }

  action janitorSweep2: bool = all {
    janitorStage == "sweep2",
    janitorStage' = "delete_index",
    data' = Set(),
    tombstone' = tombstone,
    completed' = completed,
    hasLog' = hasLog,
    hasIndex' = hasIndex,
    ordPC' = ordPC,
  }

  action janitorComplete: bool = all {
    janitorStage == "delete_index",
    janitorStage' = "idle",
    hasIndex' = false,
    completed' = true,
    tombstone' = tombstone,
    hasLog' = hasLog,
    data' = data,
    ordPC' = ordPC,
  }

  action stutter: bool = all {
    tombstone' = tombstone,
    completed' = completed,
    hasLog' = hasLog,
    hasIndex' = hasIndex,
    data' = data,
    janitorStage' = janitorStage,
    ordPC' = ordPC,
  }

  action step = any {
    { nondet r = oneOf(REASONS); markDeleted(r) },
    { nondet id = oneOf(ORD_IDS); ordCheckBeforeWrite(id) },
    { nondet id = oneOf(ORD_IDS); ordWriteWithoutRecheck(id) },
    janitorClaim,
    janitorInterrupt,
    janitorSweep1,
    janitorDeleteLog,
    janitorSweep2,
    janitorComplete,
    stutter,
  }

  val invNoOrdinaryDataAfterComplete: bool =
    (completed and writersSettled) implies and {
      data == Set(),
      not(hasLog),
      not(hasIndex),
    }
}

module mutant_erasure_skips_recheck {
  pure val MEM_IDS: Set[str] = Set("m1")
  pure val REASONS: Set[str] = Set("erasure")

  var tombstone: str
  var completed: bool
  var hasLog: bool
  var hasIndex: bool
  var memories: Set[str]
  var memBeforeDel: Set[str]
  var memAfterDel: Set[str]
  var janitorStage: str
  var memPC: str -> str

  val writersSettled: bool =
    MEM_IDS.forall(id => memPC.get(id) != "written" and memPC.get(id) != "must_withdraw")

  action init = all {
    tombstone' = "none",
    completed' = false,
    hasLog' = true,
    hasIndex' = true,
    memories' = Set(),
    memBeforeDel' = Set(),
    memAfterDel' = Set(),
    janitorStage' = "idle",
    memPC' = MEM_IDS.mapBy(_ => "idle"),
  }

  action markDeleted(reason: str): bool = all {
    REASONS.contains(reason),
    tombstone == "none",
    tombstone' = reason,
    completed' = completed,
    hasLog' = hasLog,
    hasIndex' = hasIndex,
    memories' = memories,
    memBeforeDel' = memBeforeDel,
    memAfterDel' = memAfterDel,
    janitorStage' = janitorStage,
    memPC' = memPC,
  }

  action memWrite(id: str): bool = all {
    MEM_IDS.contains(id),
    memPC.get(id) == "idle",
    memories' = memories.union(Set(id)),
    memBeforeDel' = if (tombstone == "none") memBeforeDel.union(Set(id)) else memBeforeDel,
    memAfterDel' = if (tombstone != "none") memAfterDel.union(Set(id)) else memAfterDel,
    memPC' = memPC.set(id, "written"),
    tombstone' = tombstone,
    completed' = completed,
    hasLog' = hasLog,
    hasIndex' = hasIndex,
    janitorStage' = janitorStage,
  }

  // 变异缺陷 2：误以为 erasure 保留既有 Memory，所以对 erasure 豁免 Recheck
  action memRecheckBuggy(id: str): bool = all {
    MEM_IDS.contains(id),
    memPC.get(id) == "written",
    val nextPC = if (tombstone != "none" and tombstone != "erasure") "must_withdraw" else "done"
    all {
      memPC' = memPC.set(id, nextPC),
      tombstone' = tombstone,
      completed' = completed,
      hasLog' = hasLog,
      hasIndex' = hasIndex,
      memories' = memories,
      memBeforeDel' = memBeforeDel,
      memAfterDel' = memAfterDel,
      janitorStage' = janitorStage,
    }
  }

  action memWithdraw(id: str): bool = all {
    MEM_IDS.contains(id),
    memPC.get(id) == "must_withdraw",
    memories' = memories.exclude(Set(id)),
    memPC' = memPC.set(id, "withdrawn"),
    tombstone' = tombstone,
    completed' = completed,
    hasLog' = hasLog,
    hasIndex' = hasIndex,
    memBeforeDel' = memBeforeDel,
    memAfterDel' = memAfterDel,
    janitorStage' = janitorStage,
  }

  action janitorClaim: bool = all {
    tombstone != "none",
    not(completed),
    janitorStage == "idle",
    janitorStage' = "interrupt",
    tombstone' = tombstone,
    completed' = completed,
    hasLog' = hasLog,
    hasIndex' = hasIndex,
    memories' = memories,
    memBeforeDel' = memBeforeDel,
    memAfterDel' = memAfterDel,
    memPC' = memPC,
  }

  action janitorInterrupt: bool = all {
    janitorStage == "interrupt",
    janitorStage' = "sweep1",
    tombstone' = tombstone,
    completed' = completed,
    hasLog' = hasLog,
    hasIndex' = hasIndex,
    memories' = memories,
    memBeforeDel' = memBeforeDel,
    memAfterDel' = memAfterDel,
    memPC' = memPC,
  }

  action janitorSweep1: bool = all {
    janitorStage == "sweep1",
    janitorStage' = "delete_log",
    // KeepOnRetention 跳过 memories 清扫
    memories' = memories,
    tombstone' = tombstone,
    completed' = completed,
    hasLog' = hasLog,
    hasIndex' = hasIndex,
    memBeforeDel' = memBeforeDel,
    memAfterDel' = memAfterDel,
    memPC' = memPC,
  }

  action janitorDeleteLog: bool = all {
    janitorStage == "delete_log",
    janitorStage' = "sweep2",
    hasLog' = false,
    tombstone' = tombstone,
    completed' = completed,
    hasIndex' = hasIndex,
    memories' = memories,
    memBeforeDel' = memBeforeDel,
    memAfterDel' = memAfterDel,
    memPC' = memPC,
  }

  action janitorSweep2: bool = all {
    janitorStage == "sweep2",
    janitorStage' = "delete_index",
    memories' = memories,
    tombstone' = tombstone,
    completed' = completed,
    hasLog' = hasLog,
    hasIndex' = hasIndex,
    memBeforeDel' = memBeforeDel,
    memAfterDel' = memAfterDel,
    memPC' = memPC,
  }

  action janitorComplete: bool = all {
    janitorStage == "delete_index",
    janitorStage' = "idle",
    hasIndex' = false,
    completed' = true,
    tombstone' = tombstone,
    hasLog' = hasLog,
    memories' = memories,
    memBeforeDel' = memBeforeDel,
    memAfterDel' = memAfterDel,
    memPC' = memPC,
  }

  action stutter: bool = all {
    tombstone' = tombstone,
    completed' = completed,
    hasLog' = hasLog,
    hasIndex' = hasIndex,
    memories' = memories,
    memBeforeDel' = memBeforeDel,
    memAfterDel' = memAfterDel,
    janitorStage' = janitorStage,
    memPC' = memPC,
  }

  action step = any {
    { nondet r = oneOf(REASONS); markDeleted(r) },
    { nondet id = oneOf(MEM_IDS); memWrite(id) },
    { nondet id = oneOf(MEM_IDS); memRecheckBuggy(id) },
    { nondet id = oneOf(MEM_IDS); memWithdraw(id) },
    janitorClaim,
    janitorInterrupt,
    janitorSweep1,
    janitorDeleteLog,
    janitorSweep2,
    janitorComplete,
    stutter,
  }

  val invNoLateMemorySurvived: bool =
    (completed and writersSettled) implies (memories.intersect(memAfterDel) == Set())
}
