// -*- mode: Bluespec; -*-
/**
 * yanshi A5：迁槽即换代（ADR-0046“迁槽与游标”、ADR-0051 §5.4 的停写迁槽）。
 *
 * 迁槽工具尚未实现，本规约检验的是 ADR 写下的步骤顺序，而不是代码。模型里与现有实现对应的部分：
 *   - 出站行按分片各自的位置排序（xid8 只在本库有意义），读取返回位置以下的行：internal/outbox/pgoutbox/stream.go Read
 *   - 游标的世代不符返回 outbox_epoch_changed，低于地板返回 outbox_cursor_expired：internal/outbox/stream.go StreamState.Start
 *   - 换代：新世代、地板归零，位置序列不变：pgoutbox.Store.RotateEpoch
 *   - 恢复的 H 与槽位来自服务进程自己的槽位表：internal/httpapi/outbox.go outboxShards
 *   - xinli 每个分片一份进度；两种 410 都回到 Heads；恢复只补读本片槽位上的 Session（ownsSession）；
 *     逐 ID 读取 404 记为 Gone，此后不再补读：apps/xinli/recovery、apps/xinli/delivery/consumer.go
 *
 * 抽象：
 *   - 两个分片、两个 Session。s1 在迁移的槽位 A 上，s2 的槽位 B 始终在分片 1；迁槽在 0 与 1 之间来回最多两次（迁移与回滚）。
 *   - 事务提交即可见：xid8 的可见性屏障由 pgoutbox 负责，不在本模型的问题范围内。
 *   - Session 的日志内容不随分片变化（停写期间不变），模型只记录哪些分片上有它的副本（logAt）和删除记录（tombAt）。
 *   - 每个事件都产生一条普通信封；删除产生一条删除信封（只有它带删除原因）。
 *
 * MUTANT 选择一种违反 ADR 顺序的做法；"none" 是 ADR 的协议。每个变体都必须被某条不变量抓到（见 run.sh）。
 */
module slot_migration {
  const MUTANT: str

  pure val SHARDS: Set[int] = Set(0, 1)
  pure val SESSIONS: Set[str] = Set("s1", "s2")
  pure val SLOTS: Set[str] = Set("A", "B")
  pure val MOVING_SLOT: str = "A"
  pure val SLOT: str -> str = Map("s1" -> "A", "s2" -> "B")
  pure val INIT_TABLE: str -> int = Map("A" -> 0, "B" -> 1)
  pure val MAX_SEQ: int = 2
  pure val SEQS: Set[int] = 1.to(MAX_SEQ)
  pure val MAX_MIGRATIONS: int = 2
  // 每个 Session 最多 MAX_SEQ 条普通信封与一条删除信封，单个分片的位置不会超过 6。
  pure val MAX_POS: int = 6
  pure val POS: Set[int] = 0.to(MAX_POS)
  pure val VIEWS: Set[str] = Set("new", "old")

  type Row = { sid: str, seq: int, del: bool, pos: int }

  type Kernel = {
    // 写入者与已完成部署的进程使用的槽位表
    table: str -> int,
    // 滚动部署期间，部分服务进程仍按 prevTable 定位与回答 /v1/outbox/shards
    prevTable: str -> int,
    apiStale: bool,
    seq: str -> int,
    logAt: str -> Set[int],
    tombAt: str -> Set[int],
    purged: str -> bool,
    outbox: int -> Set[Row],
    nextPos: int -> int,
    epoch: int -> int,
    floor: int -> int,
  }

  // phase: open → stopped → copied → rollout → switched → open
  type Migration = { phase: str, src: int, dst: int, rotated: bool, drained: bool, count: int }

  // 业务线消费方：每个分片一份进度（phase/epoch/pos/slots），信封与补读结果按 Session 记录。
  type Consumer = {
    phase: int -> str,   // "heads" | "recover" | "follow"
    epoch: int -> int,
    pos: int -> int,
    slots: int -> Set[str],
    seq: str -> int,      // 按 seq 补读到的位置
    got: str -> Set[int], // 收到的普通信封
    gone: str -> bool,
    reason: str -> bool,  // 收到删除信封（删除原因只在其中）
  }

  var k: Kernel
  var m: Migration
  var c: Consumer
  // 幽灵变量：迁移删除了消费方本来还会读到的删除信封。drained 记录当时是否已判定追平。
  var lost: Set[{ sid: str, drained: bool }]

  // ---- 辅助定义 ----

  pure def migrating(s: str): bool = SLOT.get(s) == MOVING_SLOT

  def owner(s: str): int = k.table.get(SLOT.get(s))

  def deleted(s: str): bool = k.tombAt.get(s) != Set()

  def viewTable(view: str): str -> int = if (view == "old") k.prevTable else k.table

  // 按某个进程的槽位表逐 ID 读取：路由到的分片有副本且没有删除记录才可见，否则 404。
  def visibleVia(t: str -> int, s: str): bool = {
    val x = t.get(SLOT.get(s))
    k.logAt.get(s).contains(x) and not(k.tombAt.get(s).contains(x))
  }

  def caughtUp(x: int): bool = and {
    c.phase.get(x) != "heads",
    c.epoch.get(x) == k.epoch.get(x),
    c.pos.get(x) >= k.nextPos.get(x),
  }

  def knows(s: str): bool = c.seq.get(s) > 0 or c.got.get(s) != Set()

  def known(s: str, n: int): bool = n <= c.seq.get(s) or c.got.get(s).contains(n)

  def affected: Set[int] = if (MUTANT == "no_target_rotation") Set(m.src) else Set(m.src, m.dst)

  // ---- 初始化 ----

  // 消费方的每个分片可能已在 Follow，也可能刚启动、先对账。
  action init = {
    nondet starts = SHARDS.setOfMaps(Set("follow", "heads")).oneOf()
    initFrom(starts)
  }

  action initFrom(starts: int -> str): bool = {
    all {
      k' = {
        table: INIT_TABLE,
        prevTable: INIT_TABLE,
        apiStale: false,
        seq: SESSIONS.mapBy(_ => 0),
        logAt: SESSIONS.mapBy(s => Set(INIT_TABLE.get(SLOT.get(s)))),
        tombAt: SESSIONS.mapBy(_ => Set()),
        purged: SESSIONS.mapBy(_ => false),
        outbox: SHARDS.mapBy(_ => Set()),
        nextPos: SHARDS.mapBy(_ => 0),
        epoch: SHARDS.mapBy(_ => 0),
        floor: SHARDS.mapBy(_ => 0),
      },
      m' = { phase: "open", src: 0, dst: 1, rotated: false, drained: false, count: 0 },
      c' = {
        phase: starts,
        epoch: SHARDS.mapBy(_ => 0),
        pos: SHARDS.mapBy(_ => 0),
        slots: SHARDS.mapBy(x => SLOTS.filter(sl => INIT_TABLE.get(sl) == x)),
        seq: SESSIONS.mapBy(_ => 0),
        got: SESSIONS.mapBy(_ => Set()),
        gone: SESSIONS.mapBy(_ => false),
        reason: SESSIONS.mapBy(_ => false),
      },
      lost' = Set(),
    }
  }

  // ---- 内核写入：只在准入开放时（停写包括 Worker、控制器与 Janitor） ----

  // 事件与普通信封同一事务写入 Session 当前所在的分片。
  action write(s: str): bool = {
    val x = owner(s)
    all {
      m.phase == "open",
      not(deleted(s)),
      k.seq.get(s) < MAX_SEQ,
      k' = { ...k,
        seq: k.seq.set(s, k.seq.get(s) + 1),
        outbox: k.outbox.set(x, k.outbox.get(x).union(Set({ sid: s, seq: k.seq.get(s) + 1, del: false, pos: k.nextPos.get(x) }))),
        nextPos: k.nextPos.set(x, k.nextPos.get(x) + 1),
      },
      m' = m, c' = c, lost' = lost,
    }
  }

  // 删除记录与删除信封同一事务写入（pglifecycle.Deletions.Mark）。
  action deleteSession(s: str): bool = {
    val x = owner(s)
    all {
      m.phase == "open",
      not(deleted(s)),
      k' = { ...k,
        tombAt: k.tombAt.set(s, k.tombAt.get(s).union(Set(x))),
        outbox: k.outbox.set(x, k.outbox.get(x).union(Set({ sid: s, seq: 0, del: true, pos: k.nextPos.get(x) }))),
        nextPos: k.nextPos.set(x, k.nextPos.get(x) + 1),
      },
      m' = m, c' = c, lost' = lost,
    }
  }

  // Janitor 按当前槽位表在 Session 所在分片删除日志与普通信封；删除信封按地板淘汰。
  action purge(s: str): bool = {
    val x = owner(s)
    all {
      m.phase == "open",
      deleted(s),
      not(k.purged.get(s)),
      k' = { ...k,
        logAt: k.logAt.set(s, k.logAt.get(s).exclude(Set(x))),
        outbox: k.outbox.set(x, k.outbox.get(x).filter(r => r.sid != s or r.del)),
        purged: k.purged.set(s, true),
      },
      m' = m, c' = c, lost' = lost,
    }
  }

  // 保留期淘汰：地板推进到安全水位以下的任意位置（72 小时的检查点在这里抽象为任意时刻）。
  action prune(x: int, f: int): bool = all {
    f > k.floor.get(x),
    f <= k.nextPos.get(x),
    k' = { ...k,
      floor: k.floor.set(x, f),
      outbox: k.outbox.set(x, k.outbox.get(x).filter(r => r.pos >= f)),
    },
    m' = m, c' = c, lost' = lost,
  }

  // ---- 迁槽：停写 → 追平 → 迁移并删除源出站行 → 部署新槽位表 → 受影响分片换代 → 删除源副本并恢复准入 ----

  action stop: bool = all {
    m.phase == "open",
    m.count < MAX_MIGRATIONS,
    m' = { ...m, phase: "stopped", src: k.table.get(MOVING_SLOT), dst: 1 - k.table.get(MOVING_SLOT) },
    k' = k, c' = c, lost' = lost,
  }

  // 运维看到各受影响分片的消费方读到空结果。追平是可选的：消费方故障时可以不等（ADR-0046 迁槽与游标第 2 条）。
  action drain: bool = all {
    m.phase == "stopped" or (MUTANT == "drain_before_stop" and m.phase == "open"),
    not(m.drained),
    SHARDS.forall(x => caughtUp(x)),
    m' = { ...m, drained: true },
    k' = k, c' = c, lost' = lost,
  }

  // 复制被迁槽位的 Session 数据与删除记录，校验后删除源分片上的出站行（普通与删除信封）。
  action copy: bool = {
    val src = m.src
    val dst = m.dst
    val removed = if (MUTANT == "keep_source_rows") Set() else k.outbox.get(src).filter(r => migrating(r.sid))
    val unread = removed.filter(r => r.del and c.phase.get(src) != "heads" and c.epoch.get(src) == k.epoch.get(src) and r.pos >= c.pos.get(src))
    all {
      m.phase == "stopped",
      k' = { ...k,
        logAt: SESSIONS.mapBy(s =>
          if (migrating(s) and k.logAt.get(s).contains(src))
            (if (MUTANT == "drop_source_at_copy") k.logAt.get(s).exclude(Set(src)).union(Set(dst))
             else k.logAt.get(s).union(Set(dst)))
          else k.logAt.get(s)),
        tombAt: SESSIONS.mapBy(s =>
          if (migrating(s) and k.tombAt.get(s).contains(src)) k.tombAt.get(s).union(Set(dst)) else k.tombAt.get(s)),
        outbox: k.outbox.set(src, k.outbox.get(src).exclude(removed)),
      },
      m' = { ...m, phase: "copied" },
      c' = c,
      lost' = lost.union(unread.map(r => { sid: r.sid, drained: m.drained })),
    }
  }

  // 部署新槽位表是滚动的：完成之前，部分服务进程仍按旧表定位并回答 /v1/outbox/shards。
  action switchTable: bool = all {
    m.phase == "copied",
    MUTANT != "rotate_before_switch" or m.rotated,
    k' = { ...k, prevTable: k.table, table: k.table.set(MOVING_SLOT, m.dst), apiStale: true },
    m' = { ...m, phase: "rollout" },
    c' = c, lost' = lost,
  }

  // 全部进程（含只读的出站与会话读取接口）报告新的 Identity()。
  action rolloutDone: bool = all {
    m.phase == "rollout",
    k' = { ...k, apiStale: false },
    m' = { ...m, phase: "switched" },
    c' = c, lost' = lost,
  }

  // RotateEpoch：新世代、地板归零；位置序列（xid8）不变。
  action rotate: bool = all {
    not(m.rotated),
    if (MUTANT == "rotate_before_switch") m.phase == "copied"
    else if (MUTANT == "rotate_during_rollout") (m.phase == "rollout" or m.phase == "switched")
    else m.phase == "switched",
    k' = { ...k,
      epoch: SHARDS.mapBy(x => if (affected.contains(x)) k.epoch.get(x) + 1 else k.epoch.get(x)),
      floor: SHARDS.mapBy(x => if (affected.contains(x)) 0 else k.floor.get(x)),
    },
    m' = { ...m, rotated: true },
    c' = c, lost' = lost,
  }

  // 恢复准入之前删除源分片上的 Session 副本：此时已没有进程按旧表读取，之后的删除只会清理新位置。
  action resume: bool = all {
    m.phase == "switched",
    m.rotated,
    k' = { ...k,
      logAt: if (MUTANT == "keep_source_copy") k.logAt
             else SESSIONS.mapBy(s => if (migrating(s)) k.logAt.get(s).exclude(Set(m.src)) else k.logAt.get(s)),
    },
    m' = { ...m, phase: "open", rotated: false, drained: false, count: m.count + 1 },
    c' = c, lost' = lost,
  }

  // ---- 消费方（xinli，每个分片一份进度） ----

  // Heads：H 取该分片当前的安全水位，槽位取自回答请求的那个进程的槽位表。
  action heads(x: int, view: str): bool = {
    val t = viewTable(view)
    all {
      c.phase.get(x) == "heads",
      view == "new" or k.apiStale,
      c' = { ...c,
        phase: c.phase.set(x, "recover"),
        epoch: c.epoch.set(x, k.epoch.get(x)),
        pos: c.pos.set(x, k.nextPos.get(x)),
        slots: c.slots.set(x, SLOTS.filter(sl => t.get(sl) == x)),
      },
      k' = k, m' = m, lost' = lost,
    }
  }

  // 枚举并按 seq 补读本片槽位上的 Session；逐 ID 读取 404 记为 Gone，已 Gone 的不再读取。
  action recover(x: int, view: str): bool = {
    val t = viewTable(view)
    all {
      c.phase.get(x) == "recover",
      view == "new" or k.apiStale,
      c' = { ...c,
        phase: c.phase.set(x, "follow"),
        seq: SESSIONS.mapBy(s =>
          if (c.slots.get(x).contains(SLOT.get(s)) and not(c.gone.get(s)) and visibleVia(t, s)) k.seq.get(s)
          else c.seq.get(s)),
        gone: SESSIONS.mapBy(s =>
          c.gone.get(s) or (c.slots.get(x).contains(SLOT.get(s)) and not(visibleVia(t, s)))),
      },
      k' = k, m' = m, lost' = lost,
    }
  }

  // 读一页：游标世代不符或低于地板时得到 410，整片进度回到 Heads；否则收下 [pos, upto) 的行。
  action read(x: int, upto: int): bool = all {
    c.phase.get(x) != "heads",
    if (c.epoch.get(x) != k.epoch.get(x) or c.pos.get(x) < k.floor.get(x))
      all {
        c' = { ...c, phase: c.phase.set(x, "heads") },
        k' = k, m' = m, lost' = lost,
      }
    else {
      val batch = k.outbox.get(x).filter(r => r.pos >= c.pos.get(x) and r.pos < upto)
      all {
        upto >= c.pos.get(x),
        upto <= k.nextPos.get(x),
        c' = { ...c,
          pos: c.pos.set(x, upto),
          got: SESSIONS.mapBy(s => c.got.get(s).union(batch.filter(r => r.sid == s and not(r.del)).map(r => r.seq))),
          reason: SESSIONS.mapBy(s => c.reason.get(s) or batch.exists(r => r.sid == s and r.del)),
          gone: SESSIONS.mapBy(s => c.gone.get(s) or batch.exists(r => r.sid == s and r.del)),
        },
        k' = k, m' = m, lost' = lost,
      }
    },
  }

  action stutter: bool = all { k' = k, m' = m, c' = c, lost' = lost }

  action step = any {
    { nondet s = SESSIONS.oneOf() any { write(s), deleteSession(s), purge(s) } },
    { nondet x = SHARDS.oneOf() nondet f = POS.oneOf() prune(x, f) },
    stop,
    drain,
    copy,
    switchTable,
    rolloutDone,
    rotate,
    resume,
    { nondet x = SHARDS.oneOf() nondet v = VIEWS.oneOf() any { heads(x, v), recover(x, v) } },
    { nondet x = SHARDS.oneOf() nondet u = POS.oneOf() read(x, u) },
    stutter,
  }

  // ---- 安全性质 ----

  // 迁槽结束、消费方在每个分片都以当前世代追到水位：
  val settled: bool = m.phase == "open" and SHARDS.forall(x => c.phase.get(x) == "follow" and caughtUp(x))

  // 1. 信封不丢：此时每个未删除 Session 的每个事件都已收到（信封或按 seq 补读）；
  //    消费方知道的已删除 Session 都已确认删除（删除信封或 404）。
  val invConverged: bool = settled implies SESSIONS.forall(s =>
    if (deleted(s)) knows(s) implies c.gone.get(s)
    else SEQS.forall(n => n <= k.seq.get(s) implies known(s, n)))

  // 2. 不误判删除：xinli 的 Gone 是永久的，只能来自真正的删除。
  val invNoFalseGone: bool = SESSIONS.forall(s => c.gone.get(s) implies deleted(s))

  // 3. 源分片无残留：Janitor 清理后，任何分片上都没有该 Session 的副本或普通信封（ADR-0015）。
  val invNoResidue: bool = SESSIONS.forall(s => k.purged.get(s) implies and {
    k.logAt.get(s) == Set(),
    SHARDS.forall(x => k.outbox.get(x).forall(r => r.sid != s or r.del)),
  })

  // 4. 出站行只在 Session 当前所在的分片上（xid8 只在本库有意义）。
  val invRowsOnOwner: bool = SHARDS.forall(x => k.outbox.get(x).forall(r => owner(r.sid) == x))

  // 5. 删除后不复活：按当前槽位表读取已删除的 Session 一律 404。
  val invNoResurrection: bool = SESSIONS.forall(s => deleted(s) implies not(visibleVia(k.table, s)))

  // 6. 判定追平后迁移，不丢掉消费方还没读到的删除信封（删除原因只在其中）。
  val invDrainKeepsReasons: bool = lost.forall(l => not(l.drained))

  // ---- 可达性见证：run.sh 要求随机模拟违反它们，证明模拟真的走到了这些状态，上面的不变量不是空洞地成立 ----

  // 迁移并回滚之后，s1 有事件，消费方各分片都已回到当前世代的 Follow。
  val witnessNoSettledRollback: bool = not(m.count == 2 and settled and k.seq.get("s1") > 0)

  // 未追平就迁移，删掉了消费方还会读到的删除信封（ADR 允许的路径）。
  val witnessNoUndrainedLoss: bool = lost.forall(l => l.drained)

  val allInvariants: bool = and {
    invConverged,
    invNoFalseGone,
    invNoResidue,
    invRowsOnOwner,
    invNoResurrection,
    invDrainKeepsReasons,
  }
}

// ADR 的协议。
module correct { import slot_migration(MUTANT = "none").* }

// 场景测试另成一个模块：测试把 initFrom 当作普通一步使用，TLC 的转换要求 init 不出现在 step 里。
module scenarios {
  import slot_migration(MUTANT = "none").*

  action initFollow = initFrom(SHARDS.mapBy(_ => "follow"))

  // 完整走一遍迁槽，消费方各分片回到 Follow。
  run migrate = stop.then(copy).then(switchTable).then(rolloutDone).then(rotate).then(resume)

  run settleAll =
    read(0, 0).then(heads(0, "new")).then(recover(0, "new"))
      .then(read(1, 0)).then(heads(1, "new")).then(recover(1, "new"))
      .then(read(0, k.nextPos.get(0))).then(read(1, k.nextPos.get(1)))

  // 未追平就迁移：s1 在源分片的两条信封被删除，目标分片换代后按 seq 补读回来。
  run undrainedEventsRecoveredTest =
    initFollow
      .then(write("s1")).then(write("s1")).then(write("s2"))
      .then(migrate)
      .then(settleAll)
      .then(all {
        assert(settled),
        assert(c.seq.get("s1") == 2),
        assert(c.got.get("s1") == Set()),
        assert(allInvariants),
        stutter,
      })

  // 先追平再迁移：删除信封送达，删除原因不丢。
  run drainedDeletionKeepsReasonTest =
    initFollow
      .then(write("s1")).then(deleteSession("s1"))
      .then(stop)
      .then(read(0, 2))
      .then(drain)
      .then(copy).then(switchTable).then(rolloutDone).then(rotate).then(resume)
      .then(settleAll)
      .then(all {
        assert(settled),
        assert(c.reason.get("s1")),
        assert(c.gone.get("s1")),
        assert(lost == Set()),
        assert(allInvariants),
        stutter,
      })

  // 未追平就迁移：删除信封随源出站行删除，消费方经 404 确认删除，拿不到原因（ADR-0046 迁槽与游标第 4 条）。
  run undrainedDeletionConfirmedBy404Test =
    initFollow
      .then(write("s1")).then(read(0, 1)).then(deleteSession("s1"))
      .then(migrate)
      .then(settleAll)
      .then(all {
        assert(settled),
        assert(c.gone.get("s1")),
        assert(not(c.reason.get("s1"))),
        assert(lost == Set({ sid: "s1", drained: false })),
        assert(allInvariants),
        stutter,
      })

  // 迁移后删除再回滚：Janitor 在新位置清理，回滚把删除记录带回，源分片没有残留、Session 不复活。
  run rollbackAfterDeletionTest =
    initFollow
      .then(write("s1"))
      .then(migrate)
      .then(deleteSession("s1")).then(purge("s1"))
      .then(migrate)
      .then(settleAll)
      .then(all {
        assert(m.count == 2),
        assert(k.table.get("A") == 0),
        assert(k.logAt.get("s1") == Set()),
        assert(c.gone.get("s1")),
        assert(allInvariants),
        stutter,
      })

  // 滚动部署期间按旧表恢复：源副本仍在，逐 ID 读取不会 404。
  run staleViewDuringRolloutTest =
    initFrom(Map(0 -> "heads", 1 -> "follow"))
      .then(write("s1"))
      .then(heads(0, "new"))
      .then(stop).then(copy).then(switchTable)
      .then(recover(0, "old"))
      .then(all {
        assert(not(c.gone.get("s1"))),
        assert(allInvariants),
        stutter,
      })
}

// 变体：各违反 ADR 的一处顺序。
module keep_source_rows { import slot_migration(MUTANT = "keep_source_rows").* }
module keep_source_copy { import slot_migration(MUTANT = "keep_source_copy").* }
module drop_source_at_copy { import slot_migration(MUTANT = "drop_source_at_copy").* }
module no_target_rotation { import slot_migration(MUTANT = "no_target_rotation").* }
module rotate_before_switch { import slot_migration(MUTANT = "rotate_before_switch").* }
module rotate_during_rollout { import slot_migration(MUTANT = "rotate_during_rollout").* }
module drain_before_stop { import slot_migration(MUTANT = "drain_before_stop").* }
