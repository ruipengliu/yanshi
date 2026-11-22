// -*- mode: Bluespec; -*-
/**
 * yanshi PoC #1: lifecycle.Recheck 删除屏障形式规约 (ADR-0015 / ADR-0050)
 *
 * 对应 Go 实现：
 *   - internal/lifecycle/lifecycle.go: Recheck / KeepsMemory
 *   - internal/janitor/janitor.go: Janitor.delete (interrupt -> sweep1 -> delete_log -> sweep2 -> delete_index -> complete)
 *   - internal/memory/service.go: Service.Save (先写、后查，不因 KeepOnRetention 豁免)
 *   - internal/presence/presence.go / internal/artifact/artifact.go: 普通 SessionData 写入先写、后查
 *
 * 模型边界说明：
 *   - 假设活写入方在写入存储后，受 context.WithoutCancel(ctx) 保护能够完成 Recheck 与 withdraw。
 *   - 建模任意写入方与删除标记、Janitor 两遍清扫及任意阶段崩溃重试的交错。
 */
module lifecycle_recheck {
  pure val ORD_IDS: Set[str] = Set("d1", "d2")
  pure val MEM_IDS: Set[str] = Set("m1", "m2")
  pure val REASONS: Set[str] = Set("end_user", "retention", "erasure")

  // 删除状态与基础设施数据
  var tombstone: str       // "none" | "end_user" | "retention" | "erasure"
  var completed: bool
  var hasLog: bool
  var hasIndex: bool
  var data: Set[str]       // 普通 app.SessionData 存储 (如 presence / artifacts)
  var memories: Set[str]   // KeepOnRetention 存储 (memories)

  // 幽灵追踪集合（只用于陈述安全性质，不参与协议控制流）
  var memBeforeDel: Set[str]
  var memAfterDel: Set[str]

  // Janitor 清扫状态机 ("idle" | "interrupt" | "sweep1" | "delete_log" | "sweep2" | "delete_index")
  var janitorStage: str

  // 写入方并发步骤 PC: "idle" | "written" | "must_withdraw" | "done" | "withdrawn"
  var ordPC: str -> str
  var memPC: str -> str

  // ---- 辅助谓词 ----

  pure def keepsMemory(r: str): bool = (r == "retention" or r == "erasure")

  val writersSettled: bool = and {
    ORD_IDS.forall(id => ordPC.get(id) != "written" and ordPC.get(id) != "must_withdraw"),
    MEM_IDS.forall(id => memPC.get(id) != "written" and memPC.get(id) != "must_withdraw"),
  }

  // ---- 初始化 ----

  action init = all {
    tombstone' = "none",
    completed' = false,
    hasLog' = true,
    hasIndex' = true,
    data' = Set(),
    memories' = Set(),
    memBeforeDel' = Set(),
    memAfterDel' = Set(),
    janitorStage' = "idle",
    ordPC' = ORD_IDS.mapBy(_ => "idle"),
    memPC' = MEM_IDS.mapBy(_ => "idle"),
  }

  // ---- 1. 标记删除 (lifecycle.Deletions.Mark) ----

  action markDeleted(reason: str): bool = all {
    REASONS.contains(reason),
    tombstone == "none",
    tombstone' = reason,
    completed' = completed,
    hasLog' = hasLog,
    hasIndex' = hasIndex,
    data' = data,
    memories' = memories,
    memBeforeDel' = memBeforeDel,
    memAfterDel' = memAfterDel,
    janitorStage' = janitorStage,
    ordPC' = ordPC,
    memPC' = memPC,
  }

  // ---- 2. 普通 SessionData 写入方：先写、后查 (lifecycle.Recheck) ----

  action ordWrite(id: str): bool = all {
    ORD_IDS.contains(id),
    ordPC.get(id) == "idle",
    data' = data.union(Set(id)),
    ordPC' = ordPC.set(id, "written"),
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

  action ordRecheck(id: str): bool = all {
    ORD_IDS.contains(id),
    ordPC.get(id) == "written",
    val nextPC = if (tombstone != "none") "must_withdraw" else "done"
    all {
      ordPC' = ordPC.set(id, nextPC),
      tombstone' = tombstone,
      completed' = completed,
      hasLog' = hasLog,
      hasIndex' = hasIndex,
      data' = data,
      memories' = memories,
      memBeforeDel' = memBeforeDel,
      memAfterDel' = memAfterDel,
      janitorStage' = janitorStage,
      memPC' = memPC,
    }
  }

  action ordWithdraw(id: str): bool = all {
    ORD_IDS.contains(id),
    ordPC.get(id) == "must_withdraw",
    // presence.Service.Put 通过 DeleteSession 整会话撤回；单键撤回如 artifact/memory 为 exclude(Set(id))。
    // 这里在保证删除标记已存在的前提下按全会话撤回与单键撤回皆满足安全不变量，且与 presence.Service 一致清空该会话在场记录。
    data' = Set(),
    ordPC' = ordPC.set(id, "withdrawn"),
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

  // ---- 3. Memory 写入方：先写、后查，不因 KeepOnRetention 豁免 Recheck ----

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
    data' = data,
    janitorStage' = janitorStage,
    ordPC' = ordPC,
  }

  action memRecheck(id: str): bool = all {
    MEM_IDS.contains(id),
    memPC.get(id) == "written",
    val nextPC = if (tombstone != "none") "must_withdraw" else "done"
    all {
      memPC' = memPC.set(id, nextPC),
      tombstone' = tombstone,
      completed' = completed,
      hasLog' = hasLog,
      hasIndex' = hasIndex,
      data' = data,
      memories' = memories,
      memBeforeDel' = memBeforeDel,
      memAfterDel' = memAfterDel,
      janitorStage' = janitorStage,
      ordPC' = ordPC,
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
    data' = data,
    memBeforeDel' = memBeforeDel,
    memAfterDel' = memAfterDel,
    janitorStage' = janitorStage,
    ordPC' = ordPC,
  }

  // ---- 4. Janitor 多阶段幂等清理 (internal/janitor/janitor.go: delete) ----

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
    memories' = memories,
    memBeforeDel' = memBeforeDel,
    memAfterDel' = memAfterDel,
    ordPC' = ordPC,
    memPC' = memPC,
  }

  action janitorInterrupt: bool = all {
    janitorStage == "interrupt",
    janitorStage' = "sweep1",
    tombstone' = tombstone,
    completed' = completed,
    hasLog' = hasLog,
    hasIndex' = hasIndex,
    data' = data,
    memories' = memories,
    memBeforeDel' = memBeforeDel,
    memAfterDel' = memAfterDel,
    ordPC' = ordPC,
    memPC' = memPC,
  }

  action janitorSweep1: bool = all {
    janitorStage == "sweep1",
    janitorStage' = "delete_log",
    data' = Set(),
    memories' = if (keepsMemory(tombstone)) memories else Set(),
    tombstone' = tombstone,
    completed' = completed,
    hasLog' = hasLog,
    hasIndex' = hasIndex,
    memBeforeDel' = memBeforeDel,
    memAfterDel' = memAfterDel,
    ordPC' = ordPC,
    memPC' = memPC,
  }

  action janitorDeleteLog: bool = all {
    janitorStage == "delete_log",
    janitorStage' = "sweep2",
    hasLog' = false,
    tombstone' = tombstone,
    completed' = completed,
    hasIndex' = hasIndex,
    data' = data,
    memories' = memories,
    memBeforeDel' = memBeforeDel,
    memAfterDel' = memAfterDel,
    ordPC' = ordPC,
    memPC' = memPC,
  }

  action janitorSweep2: bool = all {
    janitorStage == "sweep2",
    janitorStage' = "delete_index",
    data' = Set(),
    memories' = if (keepsMemory(tombstone)) memories else Set(),
    tombstone' = tombstone,
    completed' = completed,
    hasLog' = hasLog,
    hasIndex' = hasIndex,
    memBeforeDel' = memBeforeDel,
    memAfterDel' = memAfterDel,
    ordPC' = ordPC,
    memPC' = memPC,
  }

  action janitorComplete: bool = all {
    janitorStage == "delete_index",
    janitorStage' = "idle",
    hasIndex' = false,
    completed' = true,
    tombstone' = tombstone,
    hasLog' = hasLog,
    data' = data,
    memories' = memories,
    memBeforeDel' = memBeforeDel,
    memAfterDel' = memAfterDel,
    ordPC' = ordPC,
    memPC' = memPC,
  }

  // 任意清洗阶段崩溃，租约超时/释放后新 Janitor 从头重新执行幂等阶段
  action janitorCrash: bool = all {
    janitorStage != "idle",
    janitorStage' = "idle",
    tombstone' = tombstone,
    completed' = completed,
    hasLog' = hasLog,
    hasIndex' = hasIndex,
    data' = data,
    memories' = memories,
    memBeforeDel' = memBeforeDel,
    memAfterDel' = memAfterDel,
    ordPC' = ordPC,
    memPC' = memPC,
  }

  action stutter: bool = all {
    tombstone' = tombstone,
    completed' = completed,
    hasLog' = hasLog,
    hasIndex' = hasIndex,
    data' = data,
    memories' = memories,
    memBeforeDel' = memBeforeDel,
    memAfterDel' = memAfterDel,
    janitorStage' = janitorStage,
    ordPC' = ordPC,
    memPC' = memPC,
  }

  // ---- 状态迁移关系 ----

  action step = any {
    { nondet r = oneOf(REASONS); markDeleted(r) },
    { nondet id = oneOf(ORD_IDS); ordWrite(id) },
    { nondet id = oneOf(ORD_IDS); ordRecheck(id) },
    { nondet id = oneOf(ORD_IDS); ordWithdraw(id) },
    { nondet id = oneOf(MEM_IDS); memWrite(id) },
    { nondet id = oneOf(MEM_IDS); memRecheck(id) },
    { nondet id = oneOf(MEM_IDS); memWithdraw(id) },
    janitorClaim,
    janitorInterrupt,
    janitorSweep1,
    janitorDeleteLog,
    janitorSweep2,
    janitorComplete,
    janitorCrash,
    stutter,
  }

  // ---- 核心安全不变量 (ADR-0015 / ADR-0050) ----

  // 1. 删除完成前必然存在删除记录
  val invCompletedHasTombstone: bool = completed implies tombstone != "none"

  // 2. 删除完成且在途 Recheck 收敛后，普通存储、日志与索引绝无残留
  val invNoOrdinaryDataAfterComplete: bool =
    (completed and writersSettled) implies and {
      data == Set(),
      not(hasLog),
      not(hasIndex),
    }

  // 3. 普通硬删除 (end_user) 完成后，Memory 同样绝无残留
  val invNoMemoryOnHardDelete: bool =
    (completed and tombstone == "end_user" and writersSettled) implies (memories == Set())

  // 4. retention / erasure 虽跳过既有 Memory，但删除标记后的迟到写入绝不残留
  val invNoLateMemorySurvived: bool =
    (completed and writersSettled) implies (memories.intersect(memAfterDel) == Set())

  // 5. retention / erasure 不会误删删除前已完成确认的既有 Memory
  val invPreMemoryPreserved: bool =
    keepsMemory(tombstone) implies MEM_IDS.forall(id =>
      (memPC.get(id) == "done" and memBeforeDel.contains(id)) implies memories.contains(id)
    )

  val allInvariants: bool = and {
    invCompletedHasTombstone,
    invNoOrdinaryDataAfterComplete,
    invNoMemoryOnHardDelete,
    invNoLateMemorySurvived,
    invPreMemoryPreserved,
  }

  // ---- 内置单元测试 ----

  run hardDeleteRunTest = {
    init
      .then(ordWrite("d1"))
      .then(ordRecheck("d1"))
      .then(memWrite("m1"))
      .then(memRecheck("m1"))
      .then(markDeleted("end_user"))
      .then(ordWrite("d2"))
      .then(ordRecheck("d2"))
      .then(ordWithdraw("d2"))
      .then(janitorClaim)
      .then(janitorInterrupt)
      .then(janitorSweep1)
      .then(janitorDeleteLog)
      .then(janitorSweep2)
      .then(janitorComplete)
      .then(all {
        assert(allInvariants),
        assert(data == Set()),
        assert(memories == Set()),
        assert(not(hasLog) and not(hasIndex)),
        stutter,
      })
  }

  run erasureLateMemoryWithdrawnTest = {
    init
      .then(memWrite("m1"))
      .then(memRecheck("m1"))
      .then(markDeleted("erasure"))
      .then(memWrite("m2"))
      .then(janitorClaim)
      .then(janitorInterrupt)
      .then(janitorSweep1)
      .then(janitorDeleteLog)
      .then(janitorSweep2)
      .then(janitorComplete)
      .then(memRecheck("m2"))
      .then(memWithdraw("m2"))
      .then(all {
        assert(allInvariants),
        assert(memories == Set("m1")),
        assert(not(memories.contains("m2"))),
        stutter,
      })
  }

  run janitorCrashRecoveryTest = {
    init
      .then(ordWrite("d1"))
      .then(ordRecheck("d1"))
      .then(memWrite("m1"))
      .then(memRecheck("m1"))
      .then(markDeleted("retention"))
      .then(janitorClaim)
      .then(janitorInterrupt)
      .then(janitorSweep1)
      .then(janitorDeleteLog)
      .then(janitorCrash) // 删日志后、第二遍清扫前崩溃
      .then(ordWrite("d2"))
      .then(janitorClaim)
      .then(janitorInterrupt)
      .then(janitorSweep1)
      .then(janitorDeleteLog)
      .then(janitorSweep2)
      .then(janitorComplete)
      .then(ordRecheck("d2"))
      .then(ordWithdraw("d2"))
      .then(all {
        assert(allInvariants),
        assert(data == Set()),
        assert(memories == Set("m1")),
        stutter,
      })
  }
}
