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
 *   - 取 H 时记下槽位表摘要（identity，含槽位与分片列表），发现循环看到不同的摘要即把该分片重置回 Heads：
 *     delivery.Consumer.ReconcileIdentity（IDENTITY_CHECK）
 *   - /v1/outbox/shards 列出全部配置分片；移出配置的分片读取得到 404 outbox_unknown_shard，也取不到 H。
 *     发现循环看到某分片不再列出，停掉它的循环并把进度重置回 Heads：delivery.Consumer.Retire（RETIRE）
 *
 * 抽象：
 *   - 两个分片、两个 Session。s1 在迁移的槽位 A 上，s2 的槽位 B 始终在分片 B_HOME，A 起初在另一片；
 *     迁槽在 0 与 1 之间来回最多两次（迁移与回滚）。
 *   - 分片移除：A 迁走后原分片没有槽位，切换槽位表时运维可以把它移出配置（合并）；回滚时重新加入，编号不变。
 *   - 切单库：/v1/outbox/shards 只列出分片 0 时，xinli 改用单库的进度（键 -1）消费分片 0、补读全部槽位，
 *     各分片的进度由 Retire 重置；重新出现多个分片时反过来（apps/xinli/delivery/consumer.go Start）。
 *     B_HOME = 0 时合并只剩分片 0，走这条切换；B_HOME = 1 时合并只剩分片 1，不切换。
 *   - 每个进度键各自数轮次；单库的确认阶段跳过 SeenRound 等于本轮的 Session（本轮已访问），分片的进度确认全部已知 Session。
 *     分片的轮次记 SeenRound = 0（apps/xinli/recovery/recovery.go Events）。
 *   - 过期的单库进度（B_HOME = 0 的初始状态）：此前是单库（全部槽位在分片 0），xinli 停机期间扩成多分片，
 *     A 迁出分片 0、分片 0 作为源分片换代过。单库的记录留在停机时的阶段，世代是旧的；它的轮次与 SeenRound 是旧单库的，
 *     摘要是旧单库的表，合回单库后与当时相同（ABA）。重启后的 xinli 只重置本进程启动过的循环，多分片期间不会动它；
 *     模型的 read 与 retire 不区分这一点，也允许那时读到 410 或被重置（过近似），不动它的轨迹同样在内。
 *   - 非分片内核（没有槽位表配置，单库默认装配）：/v1/outbox/shards 只回答分片 0，不带槽位，摘要为空
 *     （internal/httpapi/outbox.go outboxShards）；读其他分片得到 404。全部槽位在分片 0、只配置分片 0 时，运维可以在
 *     分片的单库配置与非分片之间切换，槽位不变、不换代，部署同样是滚动的；从非分片也可以直接迁槽扩成多分片。
 *     xinli 两种回答下都按单库消费，单库补读全部 Session、不看回答里的槽位（apps/xinli/recovery/shard.go ownsSession）；
 *     空摘要与非分片的槽位表一一对应，模型把它表示为 sharded = false 的那张单库表。
 *   - 事务提交即可见：xid8 的可见性屏障由 pgoutbox 负责，不在本模型的问题范围内。
 *   - Session 的日志内容不随分片变化（停写期间不变），模型只记录哪些分片上有它的副本（logAt）和删除记录（tombAt）。
 *   - 每个事件都产生一条普通信封；删除产生一条删除信封（只有它带删除原因）。
 *
 * MUTANT 选择一种违反 ADR 顺序的做法；"none" 是 ADR 的协议。没有 identity 核对时，每个变体都必须被某条不变量抓到；
 * 有核对时，漏换代、先换代后切表、部署未完成就换代这三个变体的信封仍须收齐（见 run.sh）。
 * shard_seen_round 让分片的轮次照旧记入 SeenRound：切到单库后，单库一轮的编号与分片的轮次相同时，
 * 确认阶段跳过迁移中被删除的 Session，它的删除得不到确认（2026-11-23 修复的 xinli 缺陷）。
 * no_target_rotation_skip_removed 在漏换代之外还跳过被移出配置的源分片：回滚恢复原部署制品时摘要与移除前相同（ABA），
 * 没看到中间槽位表的消费方（例如 xinli 当时停机）核对不出变化，有核对也须被抓到。
 * keep_single_epoch 从不对分片 0 换代（扩成多分片时它是源，合回单库时它是目标）：过期的单库游标仍然有效，
 * 合回单库后摘要也与当时相同，有核对也须被抓到。
 * single_uses_slots 让单库只补读回答里列出的槽位：分片的单库配置列出全部槽位，与正确做法没有区别；
 * 非分片内核不带槽位，单库一个 Session 也不补读，须被抓到。
 */
module slot_migration {
  const MUTANT: str
  const IDENTITY_CHECK: bool
  const RETIRE: bool
  const B_HOME: int

  pure val SHARDS: Set[int] = Set(0, 1)
  pure val SESSIONS: Set[str] = Set("s1", "s2")
  pure val SLOTS: Set[str] = Set("A", "B")
  pure val MOVING_SLOT: str = "A"
  pure val SLOT: str -> str = Map("s1" -> "A", "s2" -> "B")
  pure val INIT_TABLE: str -> int = Map("A" -> 1 - B_HOME, "B" -> B_HOME)
  // 消费方的进度键：-1 是单库，其余是分片编号。
  pure val KEYS: Set[int] = Set(-1, 0, 1)
  pure def shardOf(key: int): int = if (key < 0) 0 else key
  pure val MAX_SEQ: int = 2
  pure val SEQS: Set[int] = 1.to(MAX_SEQ)
  pure val MAX_MIGRATIONS: int = 2
  // 每个 Session 最多 MAX_SEQ 条普通信封与一条删除信封，单个分片的位置不会超过 6。
  pure val MAX_POS: int = 6
  pure val POS: Set[int] = 0.to(MAX_POS)
  pure val VIEWS: Set[str] = Set("new", "old")

  type Row = { sid: str, seq: int, del: bool, pos: int }

  // 槽位表摘要所覆盖的内容（internal/shard/config.go digest）：槽位与配置中的分片；非分片内核的摘要为空（sharded = false）。
  type Ident = { table: str -> int, listed: Set[int], sharded: bool }

  type Kernel = {
    // 写入者与已完成部署的进程使用的槽位表
    table: str -> int,
    // 滚动部署期间，部分服务进程仍按 prevTable 定位与回答 /v1/outbox/shards
    prevTable: str -> int,
    // 配置中的分片（/v1/outbox/shards 列出的全部分片，失联的也列出）；prevListed 对应 prevTable
    listed: Set[int],
    prevListed: Set[int],
    // 是否有槽位表配置；false 时是非分片内核，只有分片 0、全部槽位在它上面。prevSharded 对应 prevTable
    sharded: bool,
    prevSharded: bool,
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
  // removed 是幽灵字段：曾经有分片被移出配置（供可达性见证使用）。toggles 是在分片的单库配置与非分片之间切换的次数。
  type Migration = { phase: str, src: int, dst: int, rotated: bool, drained: bool, count: int, removed: bool, toggles: int }

  // 业务线消费方：每个进度键一份进度（phase/epoch/pos/slots/round），信封与补读结果按 Session 记录。
  type Consumer = {
    phase: int -> str,   // "heads" | "recover" | "follow"
    round: int -> int,   // 每个进度键各自的轮次，取 H 时加一
    epoch: int -> int,
    pos: int -> int,
    slots: int -> Set[str],
    ident: int -> Ident, // 取 H 时回答请求的进程所用的槽位表与分片列表（摘要由它决定）
    seq: str -> int,      // 按 seq 补读到的位置
    got: str -> Set[int], // 收到的普通信封
    gone: str -> bool,
    reason: str -> bool,  // 收到删除信封（删除原因只在其中）
    seen: str -> int,     // Checkpoint.SeenRound
    staleSingle: bool,    // 幽灵字段：初始时单库有过期的进度（供可达性见证使用）
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

  def viewListed(view: str): Set[int] = if (view == "old") k.prevListed else k.listed

  def viewSharded(view: str): bool = if (view == "old") k.prevSharded else k.sharded

  def viewIdent(view: str): Ident = { table: viewTable(view), listed: viewListed(view), sharded: viewSharded(view) }

  def currentIdent: Ident = { table: k.table, listed: k.listed, sharded: k.sharded }

  // 回答里某分片列出的槽位；非分片内核不带槽位。
  def viewSlots(view: str, x: int): Set[str] =
    if (viewSharded(view)) SLOTS.filter(sl => viewTable(view).get(sl) == x) else Set()

  // 只列出分片 0 时 xinli 按单库消费（apps/xinli/delivery/consumer.go Start 的判断）。
  def single(view: str): bool = viewListed(view) == Set(0)

  // 该进程的回答下，发现循环为这个进度键运行循环。
  def active(view: str, key: int): bool =
    if (key < 0) single(view) else viewListed(view).contains(key) and not(single(view))

  // 读取请求可能落到任一服务进程：配置中有该分片的进程才能读，否则 404。
  def servable(x: int): bool = k.listed.contains(x) or (k.apiStale and k.prevListed.contains(x))

  // 按某个进程的槽位表逐 ID 读取：路由到的分片有副本且没有删除记录才可见，否则 404。
  def visibleVia(t: str -> int, s: str): bool = {
    val x = t.get(SLOT.get(s))
    k.logAt.get(s).contains(x) and not(k.tombAt.get(s).contains(x))
  }

  def caughtUp(key: int): bool = and {
    c.phase.get(key) != "heads",
    c.epoch.get(key) == k.epoch.get(shardOf(key)),
    c.pos.get(key) >= k.nextPos.get(shardOf(key)),
  }

  def knows(s: str): bool = c.seq.get(s) > 0 or c.got.get(s) != Set()

  def known(s: str, n: int): bool = n <= c.seq.get(s) or c.got.get(s).contains(n)

  // 移出配置的源分片，库在下线前仍可换代，ADR 要求照样换代（第 3 条）。
  def affected: Set[int] =
    if (MUTANT == "no_target_rotation") Set(m.src)
    else if (MUTANT == "no_target_rotation_skip_removed") Set(m.src).intersect(k.listed)
    else if (MUTANT == "keep_single_epoch") Set(m.src, m.dst).exclude(Set(0))
    else Set(m.src, m.dst)

  // 旧单库时的槽位表与分片列表：合回单库后的摘要与它相同。旧单库可能是分片的单库配置，也可能是非分片内核（摘要为空）。
  pure def singleIdent(sharded: bool): Ident = { table: SLOTS.mapBy(_ => 0), listed: Set(0), sharded: sharded }

  pure val MAX_TOGGLES: int = 2

  // ---- 初始化 ----

  // 消费方的每个分片可能已在 Follow（已完成一轮），也可能刚启动、先对账。起初是多分片；
  // B_HOME = 0 时单库可能留有过期的进度（stale 是它停下时的阶段，"none" 表示没有），旧单库第 1 轮访问过的 Session 记 SeenRound = 1。
  action init = {
    nondet starts = SHARDS.setOfMaps(Set("follow", "heads")).oneOf()
    nondet stale = (if (B_HOME == 0) Set("none", "heads", "recover", "follow") else Set("none")).oneOf()
    nondet seen = SESSIONS.setOfMaps(Set(0, 1)).oneOf()
    nondet staleSharded = Set(true, false).oneOf()
    initWith(starts, stale, staleSharded, if (stale == "none") SESSIONS.mapBy(_ => 0) else seen)
  }

  action initFrom(starts: int -> str): bool = initWith(starts, "none", true, SESSIONS.mapBy(_ => 0))

  // 过期单库进度的世代早于当前（分片 0 扩容时作为源分片换代过）；keep_single_epoch 下分片 0 从未换代，世代仍是当前的。
  // staleSharded：旧单库是分片的单库配置，还是非分片内核。
  action initWith(starts: int -> str, stale: str, staleSharded: bool, seen: str -> int): bool = {
    val old = stale != "none"
    val staleSlots = if (MUTANT == "single_uses_slots" and not(staleSharded)) Set() else SLOTS
    all {
      k' = {
        table: INIT_TABLE,
        prevTable: INIT_TABLE,
        listed: SHARDS,
        prevListed: SHARDS,
        sharded: true,
        prevSharded: true,
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
      m' = { phase: "open", src: 0, dst: 1, rotated: false, drained: false, count: 0, removed: false, toggles: 0 },
      c' = {
        phase: starts.put(-1, if (old) stale else "heads"),
        round: KEYS.mapBy(key => if (key < 0) (if (old) 1 else 0) else if (starts.get(key) == "follow") 1 else 0),
        epoch: KEYS.mapBy(key => if (key < 0 and old and MUTANT != "keep_single_epoch") -1 else 0),
        pos: KEYS.mapBy(_ => 0),
        slots: KEYS.mapBy(key => if (key < 0) (if (old) staleSlots else Set()) else SLOTS.filter(sl => INIT_TABLE.get(sl) == key)),
        ident: KEYS.mapBy(key => if (key < 0 and old) singleIdent(staleSharded) else { table: INIT_TABLE, listed: SHARDS, sharded: true }),
        seq: SESSIONS.mapBy(_ => 0),
        got: SESSIONS.mapBy(_ => Set()),
        gone: SESSIONS.mapBy(_ => false),
        reason: SESSIONS.mapBy(_ => false),
        seen: seen,
        staleSingle: old,
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
    not(k.apiStale),
    m.count < MAX_MIGRATIONS,
    m' = { ...m, phase: "stopped", src: k.table.get(MOVING_SLOT), dst: 1 - k.table.get(MOVING_SLOT) },
    k' = k, c' = c, lost' = lost,
  }

  // 运维看到各受影响分片的消费方读到空结果。追平是可选的：消费方故障时可以不等（ADR-0046 迁槽与游标第 2 条）。
  action drain: bool = all {
    m.phase == "stopped" or (MUTANT == "drain_before_stop" and m.phase == "open"),
    not(m.drained),
    KEYS.forall(key => active("new", key) implies caughtUp(key)),
    m' = { ...m, drained: true },
    k' = k, c' = c, lost' = lost,
  }

  // 复制被迁槽位的 Session 数据与删除记录，校验后删除源分片上的出站行（普通与删除信封）。
  action copy: bool = {
    val src = m.src
    val dst = m.dst
    val removed = if (MUTANT == "keep_source_rows") Set() else k.outbox.get(src).filter(r => migrating(r.sid))
    // 还会被某个进度键读到的删除信封：它在读源分片、游标有效且尚未读过该行。
    val unread = removed.filter(r => r.del and KEYS.exists(key => and {
      shardOf(key) == src, c.phase.get(key) != "heads", c.epoch.get(key) == k.epoch.get(src), r.pos >= c.pos.get(key),
    }))
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
  // 源分片因此没有槽位时，可以同时把它移出配置（drop）；目标分片若已被移出，随之重新加入。
  // 新槽位表一定是分片的配置；从非分片内核迁槽时，旧进程仍是非分片的。
  action switchTable(drop: bool): bool = {
    val t = k.table.set(MOVING_SLOT, m.dst)
    all {
      m.phase == "copied",
      MUTANT != "rotate_before_switch" or m.rotated,
      not(drop) or SLOTS.forall(sl => t.get(sl) != m.src),
      k' = { ...k,
        prevTable: k.table, table: t,
        prevListed: k.listed,
        listed: (if (drop) k.listed.exclude(Set(m.src)) else k.listed).union(Set(m.dst)),
        prevSharded: k.sharded,
        sharded: true,
        apiStale: true,
      },
      m' = { ...m, phase: "rollout", removed: m.removed or drop },
      c' = c, lost' = lost,
    }
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

  // 只配置分片 0、全部槽位在它上面时，在分片的单库配置与非分片之间切换：槽位与数据不变，不停写、不换代；
  // 部署是滚动的，完成之前两种进程交替回答，只有摘要不同。
  action toggleSharding: bool = all {
    m.phase == "open",
    not(k.apiStale),
    k.listed == Set(0),
    m.toggles < MAX_TOGGLES,
    k' = { ...k, prevTable: k.table, prevListed: k.listed, prevSharded: k.sharded, sharded: not(k.sharded), apiStale: true },
    m' = { ...m, toggles: m.toggles + 1 },
    c' = c, lost' = lost,
  }

  action toggleDone: bool = all {
    m.phase == "open",
    k.apiStale,
    k' = { ...k, apiStale: false },
    m' = m, c' = c, lost' = lost,
  }

  // ---- 消费方（xinli，每个进度键一份进度：分片编号，或单库的 -1） ----

  // Heads：开始新一轮，H 取所读分片当前的安全水位；槽位取自回答请求的那个进程的槽位表，单库补读全部槽位
  // （single_uses_slots 只取回答里分片 0 列出的槽位）。
  // 该进程的回答下这个键没有循环（分片未列出，或单库与多分片不符）时取不到 H。
  action heads(key: int, view: str): bool = {
    val t = viewTable(view)
    val x = shardOf(key)
    all {
      c.phase.get(key) == "heads",
      view == "new" or k.apiStale,
      active(view, key),
      c' = { ...c,
        phase: c.phase.set(key, "recover"),
        round: c.round.set(key, c.round.get(key) + 1),
        epoch: c.epoch.set(key, k.epoch.get(x)),
        pos: c.pos.set(key, k.nextPos.get(x)),
        slots: c.slots.set(key,
          if (key >= 0) SLOTS.filter(sl => t.get(sl) == key)
          else if (MUTANT == "single_uses_slots") viewSlots(view, 0)
          else SLOTS),
        ident: c.ident.set(key, viewIdent(view)),
      },
      k' = k, m' = m, lost' = lost,
    }
  }

  // 枚举并按 seq 补读本键槽位上的 Session，访问过的记下 SeenRound；逐 ID 读取 404 记为 Gone，已 Gone 的不再读取。
  // 删除了的 Session 不会被枚举到，只能在确认阶段读到 404：单库跳过 SeenRound 等于本轮的，分片确认全部已知的。
  action recover(key: int, view: str): bool = {
    val t = viewTable(view)
    val sl = c.slots.get(key)
    val r = c.round.get(key)
    all {
      c.phase.get(key) == "recover",
      view == "new" or k.apiStale,
      c' = { ...c,
        phase: c.phase.set(key, "follow"),
        seq: SESSIONS.mapBy(s =>
          if (sl.contains(SLOT.get(s)) and not(c.gone.get(s)) and visibleVia(t, s)) k.seq.get(s)
          else c.seq.get(s)),
        seen: SESSIONS.mapBy(s =>
          if (sl.contains(SLOT.get(s)) and not(c.gone.get(s)) and visibleVia(t, s))
            (if (key < 0 or MUTANT == "shard_seen_round") r else 0)
          else c.seen.get(s)),
        gone: SESSIONS.mapBy(s =>
          c.gone.get(s) or and {
            sl.contains(SLOT.get(s)),
            not(visibleVia(t, s)),
            key >= 0 or c.seen.get(s) != r,
          }),
      },
      k' = k, m' = m, lost' = lost,
    }
  }

  // 发现循环：回答 /v1/outbox/shards 的进程的摘要与取 H 时不同，就把有循环的键重置回 Heads；在 Heads 时不动。
  action reconcile(key: int, view: str): bool = all {
    IDENTITY_CHECK,
    c.phase.get(key) != "heads",
    view == "new" or k.apiStale,
    active(view, key),
    c.ident.get(key) != viewIdent(view),
    c' = { ...c, phase: c.phase.set(key, "heads") },
    k' = k, m' = m, lost' = lost,
  }

  // 发现循环：该进程的回答下这个键不再有循环（分片未列出，或切换了单库与多分片），停掉循环并把进度重置回 Heads。
  action retire(key: int, view: str): bool = all {
    RETIRE,
    c.phase.get(key) != "heads",
    view == "new" or k.apiStale,
    not(active(view, key)),
    c' = { ...c, phase: c.phase.set(key, "heads") },
    k' = k, m' = m, lost' = lost,
  }

  // 读一页：分片不在配置中时得到 404，什么都不变；游标世代不符或低于地板时得到 410，进度回到 Heads；
  // 否则收下 [pos, upto) 的行。尚未停下的循环也可能读（发现循环每秒一次）。
  action read(key: int, upto: int): bool = {
    val x = shardOf(key)
    all {
    c.phase.get(key) != "heads",
    servable(x),
    if (c.epoch.get(key) != k.epoch.get(x) or c.pos.get(key) < k.floor.get(x))
      all {
        c' = { ...c, phase: c.phase.set(key, "heads") },
        k' = k, m' = m, lost' = lost,
      }
    else {
      val batch = k.outbox.get(x).filter(r => r.pos >= c.pos.get(key) and r.pos < upto)
      all {
        upto >= c.pos.get(key),
        upto <= k.nextPos.get(x),
        c' = { ...c,
          pos: c.pos.set(key, upto),
          got: SESSIONS.mapBy(s => c.got.get(s).union(batch.filter(r => r.sid == s and not(r.del)).map(r => r.seq))),
          reason: SESSIONS.mapBy(s => c.reason.get(s) or batch.exists(r => r.sid == s and r.del)),
          gone: SESSIONS.mapBy(s => c.gone.get(s) or batch.exists(r => r.sid == s and r.del)),
        },
        k' = k, m' = m, lost' = lost,
      }
    },
    }
  }

  action stutter: bool = all { k' = k, m' = m, c' = c, lost' = lost }

  action step = any {
    { nondet s = SESSIONS.oneOf() any { write(s), deleteSession(s), purge(s) } },
    { nondet x = SHARDS.oneOf() nondet f = POS.oneOf() prune(x, f) },
    stop,
    drain,
    copy,
    { nondet drop = Set(true, false).oneOf() switchTable(drop) },
    rolloutDone,
    toggleSharding,
    toggleDone,
    rotate,
    resume,
    { nondet key = KEYS.oneOf() nondet v = VIEWS.oneOf() any { heads(key, v), recover(key, v), reconcile(key, v), retire(key, v) } },
    { nondet key = KEYS.oneOf() nondet u = POS.oneOf() read(key, u) },
    stutter,
  }

  // ---- 安全性质 ----

  // 迁槽结束、消费方在当前有循环的每个进度键上都以当前世代追到水位：
  // 有 identity 核对时，还须都已看到当前的槽位表：否则发现循环还会触发新一轮对账，消费方没有停下。
  // 没有循环的键（移出配置的分片、切换后不用的单库或分片进度）不再要求任何进度；有 Retire 时须已重置回 Heads。
  val settled: bool = m.phase == "open" and KEYS.forall(key =>
    if (active("new", key)) and {
      c.phase.get(key) == "follow",
      caughtUp(key),
      IDENTITY_CHECK implies c.ident.get(key) == currentIdent,
    }
    else RETIRE implies c.phase.get(key) == "heads")

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

  // 7. 只移除没有槽位的分片：槽位表只指向配置中的分片。
  val invTableListed: bool = SLOTS.forall(sl => k.listed.contains(k.table.get(sl)))

  // 8. 非分片内核只有分片 0。
  val invUnshardedSingle: bool = (not(k.sharded) implies k.listed == Set(0)) and (not(k.prevSharded) implies k.prevListed == Set(0))

  // ---- 可达性见证：run.sh 要求随机模拟违反它们，证明模拟真的走到了这些状态，上面的不变量不是空洞地成立 ----

  // 迁移并回滚之后，s1 有事件，消费方各分片都已回到当前世代的 Follow。
  val witnessNoSettledRollback: bool = not(m.count == 2 and settled and k.seq.get("s1") > 0)

  // 未追平就迁移，删掉了消费方还会读到的删除信封（ADR 允许的路径）。
  val witnessNoUndrainedLoss: bool = lost.forall(l => l.drained)

  // 迁移之后 s1 有事件，消费方各分片都已追平（用于有 identity 核对的变体，证明信封收齐不是因为从未追平）。
  val witnessNoSettledMigration: bool = not(m.count >= 1 and settled and k.seq.get("s1") > 0)

  // 分片 0 被移出配置后，s1 有事件，消费方已追平（收齐靠接收分片，不靠被移除分片的旧进度）。
  val witnessNoSettledRemoval: bool = not(not(k.listed.contains(0)) and settled and k.seq.get("s1") > 0)

  // 切到单库后 s1 有事件，消费方已追平（B_HOME = 0）。
  val witnessNoSettledSingle: bool = not(k.listed == Set(0) and settled and k.seq.get("s1") > 0)

  // 切到单库后，s1 在合并时被删除、删除信封丢失，单库一轮经 404 确认删除（shard_seen_round 抓的正是这条路径）。
  val witnessNoSingleConfirm: bool = not(k.listed == Set(0) and settled and c.gone.get("s1") and not(c.reason.get("s1")))

  // 分片 0 被移除又以同一编号重新加入（回滚）后，s1 有事件，消费方已追平。
  val witnessNoSettledReadd: bool = not(m.removed and m.count == 2 and settled and k.seq.get("s1") > 0)

  // 非分片内核下 s1 有事件，消费方已追平（B_HOME = 0）。
  val witnessNoSettledUnsharded: bool = not(not(k.sharded) and settled and k.seq.get("s1") > 0)

  // 切到过非分片内核之后又迁槽扩回多分片，s1 有事件，消费方已追平。
  val witnessNoSettledAfterUnsharded: bool = not(m.toggles >= 1 and k.listed == SHARDS and settled and k.seq.get("s1") > 0)

  // 初始时单库有过期的进度，合回单库后 s1 有事件，消费方已追平（B_HOME = 0）。
  val witnessNoStaleSingleSettled: bool = not(c.staleSingle and k.listed == Set(0) and settled and k.seq.get("s1") > 0)

  val allInvariants: bool = and {
    invConverged,
    invNoFalseGone,
    invNoResidue,
    invRowsOnOwner,
    invNoResurrection,
    invDrainKeepsReasons,
    invTableListed,
    invUnshardedSingle,
  }
}

// ADR 的协议。
module correct { import slot_migration(MUTANT = "none", IDENTITY_CHECK = true, RETIRE = true, B_HOME = 1).* }

// 只有 ADR 的换代、没有消费端核对：协议本身也须成立。
module correct_without_identity { import slot_migration(MUTANT = "none", IDENTITY_CHECK = false, RETIRE = true, B_HOME = 1).* }

// 合并只剩分片 0，xinli 切到单库（A 起初在分片 1，B 在分片 0）。
module correct_single { import slot_migration(MUTANT = "none", IDENTITY_CHECK = true, RETIRE = true, B_HOME = 0).* }

module single_scenarios {
  import slot_migration(MUTANT = "none", IDENTITY_CHECK = true, RETIRE = true, B_HOME = 0).*

  // 分片 1 第 1 轮访问过 s1；s1 随后被删除，未追平就把 A 并入分片 0，删除信封随源出站行删除。
  // 只剩分片 0，xinli 重置两个分片的进度、开始单库的第 1 轮：轮次与分片 1 相同，但分片的轮次不记入 SeenRound，
  // 确认阶段仍经 404 确认 s1 的删除。回滚后重新出现两个分片，单库的进度重置，各分片从新的 H 对账。
  run mergeToSingleConfirmsDeletionTest =
    initFrom(Map(0 -> "follow", 1 -> "heads"))
      .then(write("s1"))
      .then(heads(1, "new")).then(recover(1, "new"))
      .then(deleteSession("s1"))
      .then(stop).then(copy).then(switchTable(true)).then(rolloutDone).then(rotate).then(resume)
      .then(all {
        assert(k.listed == Set(0)),
        assert(c.round.get(1) == 1),
        retire(0, "new"),
      })
      .then(retire(1, "new"))
      .then(heads(-1, "new")).then(recover(-1, "new")).then(read(-1, k.nextPos.get(0)))
      .then(all {
        assert(c.round.get(-1) == 1),
        assert(settled),
        assert(c.gone.get("s1")),
        assert(not(c.reason.get("s1"))),
        assert(allInvariants),
        stutter,
      })
      .then(stop).then(copy).then(switchTable(false)).then(rolloutDone).then(rotate).then(resume)
      .then(retire(-1, "new"))
      .then(heads(0, "new")).then(recover(0, "new")).then(read(0, k.nextPos.get(0)))
      .then(heads(1, "new")).then(recover(1, "new")).then(read(1, k.nextPos.get(1)))
      .then(all {
        assert(k.listed == SHARDS),
        assert(settled),
        assert(c.phase.get(-1) == "heads"),
        assert(allInvariants),
        stutter,
      })

  // 单库第 1 轮访问过 s1 后 xinli 停机，期间扩成多分片（A 在分片 1）；重启后分片 1 尚未对账，单库的记录仍在 Follow。
  // s1 有事件后被删除，未追平就合回单库，删除信封随源出站行删除。单库的摘要与停机前相同，核对不出变化；
  // 旧游标因分片 0 换代得到 410，新一轮编号大于旧的 SeenRound，确认阶段经 404 确认 s1 的删除，按 seq 补读 s2。
  run staleSingleRecordTest =
    initWith(Map(0 -> "follow", 1 -> "heads"), "follow", true, SESSIONS.mapBy(_ => 1))
      .then(write("s1")).then(write("s2"))
      .then(deleteSession("s1"))
      .then(stop).then(copy).then(switchTable(true)).then(rolloutDone).then(rotate).then(resume)
      .then(all {
        assert(k.listed == Set(0)),
        assert(c.phase.get(-1) == "follow"),
        assert(c.ident.get(-1) == currentIdent),
        retire(0, "new"),
      })
      .then(read(-1, k.nextPos.get(0)))
      .then(all {
        assert(c.phase.get(-1) == "heads"),
        assert(not(c.gone.get("s1"))),
        heads(-1, "new"),
      })
      .then(recover(-1, "new")).then(read(-1, k.nextPos.get(0)))
      .then(all {
        assert(c.round.get(-1) == 2),
        assert(settled),
        assert(c.gone.get("s1")),
        assert(not(c.reason.get("s1"))),
        assert(c.seq.get("s2") == 1),
        assert(allInvariants),
        stutter,
      })

  // 合并只剩分片 0 后改成非分片内核：滚动部署期间两种进程交替回答，摘要不同使单库重新对账；
  // 非分片的回答不带槽位，单库仍补读全部 Session。随后从非分片直接迁槽扩回两个分片，旧进程仍按非分片回答。
  run unshardedKernelTest =
    initFrom(Map(0 -> "follow", 1 -> "follow"))
      .then(write("s1")).then(write("s2"))
      .then(stop).then(copy).then(switchTable(true)).then(rolloutDone).then(rotate).then(resume)
      .then(retire(0, "new")).then(retire(1, "new"))
      .then(heads(-1, "new")).then(recover(-1, "new")).then(read(-1, k.nextPos.get(0)))
      .then(all { assert(settled), toggleSharding })
      .then(all {
        assert(not(k.sharded) and k.prevSharded),
        assert(not(settled)),
        reconcile(-1, "new"),
      })
      // 仍是分片配置的进程回答：取到的是分片的摘要，部署完成后再对账一次。
      .then(heads(-1, "old")).then(recover(-1, "old"))
      .then(toggleDone)
      .then(reconcile(-1, "new"))
      .then(write("s1"))
      .then(heads(-1, "new")).then(recover(-1, "new")).then(read(-1, k.nextPos.get(0)))
      .then(all {
        assert(settled),
        assert(viewSlots("new", 0) == Set()),
        assert(c.slots.get(-1) == SLOTS),
        assert(c.ident.get(-1) == singleIdent(false)),
        assert(c.seq.get("s1") == 2),
        assert(allInvariants),
        stutter,
      })
      .then(stop).then(copy).then(switchTable(false))
      .then(all {
        assert(k.sharded and not(k.prevSharded)),
        assert(k.listed == SHARDS),
        retire(-1, "new"),
      })
      .then(rolloutDone).then(rotate).then(resume)
      .then(heads(0, "new")).then(recover(0, "new")).then(read(0, k.nextPos.get(0)))
      .then(heads(1, "new")).then(recover(1, "new")).then(read(1, k.nextPos.get(1)))
      .then(all {
        assert(settled),
        assert(c.phase.get(-1) == "heads"),
        assert(c.seq.get("s1") == 2),
        assert(allInvariants),
        stutter,
      })
}

// 场景测试另成一个模块：测试把 initFrom 当作普通一步使用，TLC 的转换要求 init 不出现在 step 里。
module scenarios {
  import slot_migration(MUTANT = "none", IDENTITY_CHECK = true, RETIRE = true, B_HOME = 1).*

  action initFollow = initFrom(SHARDS.mapBy(_ => "follow"))

  // 完整走一遍迁槽，消费方各分片回到 Follow。
  run migrate = stop.then(copy).then(switchTable(false)).then(rolloutDone).then(rotate).then(resume)

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
      .then(copy).then(switchTable(false)).then(rolloutDone).then(rotate).then(resume)
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
      .then(stop).then(copy).then(switchTable(false))
      .then(recover(0, "old"))
      .then(all {
        assert(not(c.gone.get("s1"))),
        assert(allInvariants),
        stutter,
      })

  // 合并：A 迁到分片 1，分片 0 没有槽位而移出配置，读取 404、取不到 H。消费方重置分片 0 的进度，
  // s1 迁移前的事件由换代后的分片 1 按新槽位补读；回滚时分片 0 以同一编号重新加入，从新的 H 对账。
  run mergeAndReaddTest =
    initFollow
      .then(write("s1")).then(write("s1"))
      .then(stop).then(copy).then(switchTable(true)).then(rolloutDone).then(rotate).then(resume)
      .then(all {
        assert(not(k.listed.contains(0))),
        assert(not(servable(0))),
        retire(0, "new"),
      })
      .then(read(1, 0)).then(heads(1, "new")).then(recover(1, "new")).then(read(1, k.nextPos.get(1)))
      .then(all {
        assert(settled),
        assert(c.phase.get(0) == "heads"),
        assert(c.slots.get(1) == Set("A", "B")),
        assert(c.seq.get("s1") == 2),
        assert(allInvariants),
        stutter,
      })
      .then(migrate)
      .then(heads(0, "new")).then(recover(0, "new"))
      .then(read(1, 0)).then(heads(1, "new")).then(recover(1, "new"))
      .then(read(0, k.nextPos.get(0))).then(read(1, k.nextPos.get(1)))
      .then(all {
        assert(m.count == 2),
        assert(k.listed == SHARDS),
        assert(settled),
        assert(c.slots.get(0) == Set("A")),
        assert(allInvariants),
        stutter,
      })
}

// 变体：各违反 ADR 的一处顺序，消费端没有 identity 核对（有分片移除的处理）。
module keep_source_rows { import slot_migration(MUTANT = "keep_source_rows", IDENTITY_CHECK = false, RETIRE = true, B_HOME = 1).* }
module keep_source_copy { import slot_migration(MUTANT = "keep_source_copy", IDENTITY_CHECK = false, RETIRE = true, B_HOME = 1).* }
module drop_source_at_copy { import slot_migration(MUTANT = "drop_source_at_copy", IDENTITY_CHECK = false, RETIRE = true, B_HOME = 1).* }
module no_target_rotation { import slot_migration(MUTANT = "no_target_rotation", IDENTITY_CHECK = false, RETIRE = true, B_HOME = 1).* }
module rotate_before_switch { import slot_migration(MUTANT = "rotate_before_switch", IDENTITY_CHECK = false, RETIRE = true, B_HOME = 1).* }
module rotate_during_rollout { import slot_migration(MUTANT = "rotate_during_rollout", IDENTITY_CHECK = false, RETIRE = true, B_HOME = 1).* }
// 分片的轮次照旧记入 SeenRound（修复前的 xinli）：切到单库后确认阶段跳过迁移中被删除的 Session。
module shard_seen_round { import slot_migration(MUTANT = "shard_seen_round", IDENTITY_CHECK = true, RETIRE = true, B_HOME = 0).* }
// 分片 0 从不换代：过期的单库游标在合回单库后仍有效，摘要也与当时相同，有核对仍丢信封。
module keep_single_epoch { import slot_migration(MUTANT = "keep_single_epoch", IDENTITY_CHECK = true, RETIRE = true, B_HOME = 0).* }
// 单库只补读回答里列出的槽位：非分片内核不带槽位，单库什么也不补读。
module single_uses_slots { import slot_migration(MUTANT = "single_uses_slots", IDENTITY_CHECK = true, RETIRE = true, B_HOME = 0).* }
module drain_before_stop { import slot_migration(MUTANT = "drain_before_stop", IDENTITY_CHECK = false, RETIRE = true, B_HOME = 1).* }

// 同样的变体，消费端有 identity 核对（xinli 的现状）：核对是换代之外的第二道防线。
module no_target_rotation_identity { import slot_migration(MUTANT = "no_target_rotation", IDENTITY_CHECK = true, RETIRE = true, B_HOME = 1).* }
module rotate_before_switch_identity { import slot_migration(MUTANT = "rotate_before_switch", IDENTITY_CHECK = true, RETIRE = true, B_HOME = 1).* }
// 摘要 ABA：回滚到与移除前相同的槽位表，核对看不出变化，被移除的源分片没有换代时信封丢失。
module no_target_rotation_skip_removed_identity { import slot_migration(MUTANT = "no_target_rotation_skip_removed", IDENTITY_CHECK = true, RETIRE = true, B_HOME = 1).* }
module rotate_during_rollout_identity {
  import slot_migration(MUTANT = "rotate_during_rollout", IDENTITY_CHECK = true, RETIRE = true, B_HOME = 1).*

  // 部署未完成就换代：分片 1 按旧槽位完成对账、带着新世代的游标进入 Follow，s1 迁移前的事件没有送达；
  // 部署完成后摘要变化把它带回对账，按 seq 补读。
  run staleSlotsRecoveredByIdentityTest =
    initFrom(Map(0 -> "follow", 1 -> "heads"))
      .then(write("s1"))
      .then(stop).then(copy).then(switchTable(false))
      .then(rotate)
      // 仍按旧表回答的进程给出新世代的 H 与旧槽位：分片 1 不认领 s1。
      .then(heads(1, "old")).then(recover(1, "old"))
      .then(rolloutDone).then(resume)
      .then(read(0, 0)).then(heads(0, "new")).then(recover(0, "new"))
      .then(all {
        assert(c.seq.get("s1") == 0),
        assert(not(settled)),
        reconcile(1, "new"),
      })
      .then(heads(1, "new")).then(recover(1, "new"))
      .then(all {
        assert(settled),
        assert(c.seq.get("s1") == 1),
        assert(allInvariants),
        stutter,
      })
}
