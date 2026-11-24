// -*- mode: Bluespec; -*-
/**
 * yanshi 外部循环：一步的提交、重试与接管（ADR-0041、ADR-0062 第 2–5 条）。
 *
 * 外部循环尚未实现，本规约检验 ADR-0062 写下的协议，不是代码；实现时补上由模型调度驱动的 Go 场景（验证方案第 3 节）。
 * 模型里与现有实现对应的部分：
 *   - 提交以 Attempt 隔离：Worker 只在投影中仍持有本 Attempt 时追加，冲突后同步再决定：
 *     internal/runtime/worker.go 的 tryCommit 与 owns；Apply 的 fencedRun 再查一次。
 *   - 开始、挂起后恢复与接管都是一条 AttemptStarted，Attempt 号加一：Worker.startAttempt。
 *   - 回答写入日志之前送检：审核不可用时回答丢弃、Run 挂起（ADR-0054）；拦截时以拒答替换回复、丢弃调用并结束 Run：
 *     Worker.callModel 的内容安全段。外部循环的状态在拦截时不写入（ADR-0062 第 5 条）。
 *   - 不合法的回答与模型错误同口径：同一 Attempt 连续出错达到上限时 Run 失败：Worker.modelFailed、MaxModelErrors。
 *   - 调用只来自已写入日志的回答，由持有 Attempt 的 Worker 执行：Worker.advanceCall。
 *   - 压缩的截断点在闭合边界上（session.State.CanCut）、不含最后一个事件（runtime.pickCut），调用进行中不压缩。
 *     外部循环另加"不越过本 Run 最近一条回答"（ADR-0062 第 5 条，新增）。
 *
 * 抽象：
 *   - 一个 Session，至多两个先后进行的 Run（第二个在第一个终态后由新输入开启；重答对状态的要求与新 Run 相同）。
 *   - 两个 Worker。任一 Worker 可以随时为活跃 Run 开始新的 Attempt，过近似了租约过期；被接管的 Worker 不知情，
 *     继续拿旧 Attempt 发请求、收回答、尝试提交。Worker 发请求前同步过日志，但同步与发出之间日志可以变化。
 *   - 外部服务无状态，回答任意：0–2 个调用与一个新的状态令牌，或者不合法（调用了请求以外的工具、状态超限、无法解析、
 *     超时，一律同口径）。状态用整数令牌表示，0 是空；调用 ID 是平台改写后的 ID。
 *   - 插话（以及迟到的后台结果、取消与通话转写）在活跃 Run 中随时到达，只是日志中的一个事件。调用进行中到达的输入
 *     在请求的 messages 中推迟到结果之后（runtime.Transcript），属于渲染，不在本模型中。
 *   - 调用结果的成败（is_error）不影响协议：每个调用恰有一条结果；执行与结果的写入合为一步。
 *   - 不建模：前缀缓存、计量、事件流对外剥离状态、按 Session 删除（make sim 与 lifecycle 模型覆盖）、外部服务自身的
 *     去重与副作用（一致性套件 looptest 检查）。
 *
 * MUTANT 选择一种违反 ADR-0062 的做法，"none" 是 ADR 的协议；每个变体都必须被指定的不变量抓到（见 run.sh）。
 */
module external_loop {
  const MUTANT: str

  pure val WORKERS: Set[str] = Set("w1", "w2")
  pure val EMPTY: int = 0
  pure val MAX_RUNS: int = 2
  pure val MAX_TURNS: int = 3
  pure val MAX_ATTEMPTS: int = 4
  pure val MAX_ERRORS: int = 2
  pure val MAX_INPUTS: int = 2
  pure val MAX_COMPACTIONS: int = 2
  pure val MAX_CALLS: int = 2
  pure val RUNS: Set[int] = 1.to(MAX_RUNS)

  // 日志中的事件。kind："requested"（RunRequested）、"attempt"（AttemptStarted）、"am"（AssistantMessage）、
  // "result"（ToolResult）、"input"（Steered 等两步之间的输入）、"suspended"、"completed"、"failed"。
  // am 的 reqTurn、reqState 与 valid 是幽灵字段：产生这条回答的请求的轮次与状态、回答是否合法。
  type Ev = {
    kind: str, rid: int, attempt: int,
    turn: int, reqTurn: int, reqState: int, state: int, calls: Set[int], blocked: bool, valid: bool,
    call: int,
  }

  pure val NOEV: Ev = {
    kind: "", rid: 0, attempt: 0,
    turn: 0, reqTurn: 0, reqState: EMPTY, state: EMPTY, calls: Set(), blocked: false, valid: true,
    call: 0,
  }

  pure def ev(kind: str, rid: int, attempt: int): Ev = { ...NOEV, kind: kind, rid: rid, attempt: attempt }

  // 一次请求（幽灵记录）。prevCalls 是上一条回答的调用，prevResults 是其中已有结果的：请求中"上一步"字段的内容。
  type Req = { rid: int, attempt: int, turn: int, state: int, prevCalls: Set[int], prevResults: Set[int] }

  type Resp = { valid: bool, calls: Set[int], state: int }

  // phase："idle"（未持有 Attempt）→ "ready" → "waiting"（请求在途）→ "got"（回答已到、尚未处理）→ "ready" 或 "idle"。
  // mem 是 Worker 进程内记住的最近一次回答的状态，只有变体 worker_memory_state 使用它；崩溃即丢失。
  type Worker = { attempt: int, rid: int, phase: str, req: Req, resp: Resp, errors: int, mem: int }

  pure val NOREQ: Req = { rid: 0, attempt: 0, turn: 0, state: EMPTY, prevCalls: Set(), prevResults: Set() }
  pure val NORESP: Resp = { valid: true, calls: Set(), state: EMPTY }
  pure val IDLE: Worker = { attempt: 0, rid: 0, phase: "idle", req: NOREQ, resp: NORESP, errors: 0, mem: EMPTY }

  // 幽灵状态：执行过的调用、发出过的请求、可达性见证的标记、压缩次数。
  type Ghost = { executed: Set[int], requests: List[Req], seen: Set[str], compactions: int }

  var log: List[Ev]
  // 最近一次压缩的截断点（log 的下标，含），-1 表示没有压缩过。
  var through: int
  var workers: str -> Worker
  // 调用 ID 与状态令牌共用的计数器，保证每个都是新的。
  var nextId: int
  var ghost: Ghost

  // ---- 日志的投影 ----

  def runEvents(r: int): List[Ev] = log.select(e => e.rid == r)

  val numRuns: int = log.select(e => e.kind == "requested").length()

  def status(r: int): str = runEvents(r).foldl("none", (s, e) =>
    if (e.kind == "requested") "queued"
    else if (e.kind == "attempt") "running"
    else if (e.kind == "suspended") "suspended"
    else if (e.kind == "completed") "completed"
    else if (e.kind == "failed") "failed"
    else s)

  def terminal(r: int): bool = status(r) == "completed" or status(r) == "failed"

  // 活跃 Run 的编号，没有时为 0。
  val active: int = if (numRuns > 0 and not(terminal(numRuns))) numRuns else 0

  def curAttempt(r: int): int = runEvents(r).foldl(0, (a, e) => if (e.kind == "attempt") e.attempt else a)

  def ams(r: int): List[Ev] = runEvents(r).select(e => e.kind == "am")

  def turns(r: int): int = ams(r).length()

  def lastAm(r: int): Ev = ams(r).foldl(NOEV, (_, e) => e)

  def results(r: int): Set[int] = runEvents(r).foldl(Set(), (s, e) => if (e.kind == "result") s.union(Set(e.call)) else s)

  def pending(r: int): Set[int] = lastAm(r).calls.exclude(results(r))

  def lastAmIdx(r: int): int = log.indices().fold(-1, (m, i) => if (log[i].kind == "am" and log[i].rid == r and i > m) i else m)

  val inputs: int = log.select(e => e.kind == "input").length()

  def owns(w: str): bool = {
    val wk = workers.get(w)
    wk.attempt > 0 and curAttempt(wk.rid) == wk.attempt and status(wk.rid) == "running"
  }

  // 整个 Session 最近一条回答的状态：变体 cross_run_state 误把它带进新 Run。
  val sessionState: int = log.foldl(EMPTY, (s, e) => if (e.kind == "am") e.state else s)

  // 下标 i 及之前的回答中的调用。
  def callsThrough(i: int): Set[int] = log.indices().filter(j => j <= i and log[j].kind == "am").map(j => log[j].calls).flatten()

  // 截断点 i 是否可用：在上次之后、不含最后一个事件、闭合边界，调用进行中不压缩；
  // 由外部循环驱动的 Run 另要求截断点在本 Run 最近一条回答之前。
  def canCompact(i: int): bool = {
    val r = active
    and {
      r > 0,
      status(r) == "running",
      pending(r) == Set(),
      ghost.compactions < MAX_COMPACTIONS,
      i > through,
      i <= log.length() - 2,
      log.indices().forall(j => (j > i and log[j].kind == "result") implies not(callsThrough(i).contains(log[j].call))),
      MUTANT == "builtin_compaction" or lastAmIdx(r) < 0 or i < lastAmIdx(r),
    }
  }

  // ---- 动作 ----

  action init = all {
    log' = [ev("requested", 1, 0)],
    through' = -1,
    workers' = WORKERS.mapBy(_ => IDLE),
    nextId' = 1,
    ghost' = { executed: Set(), requests: List(), seen: Set(), compactions: 0 },
  }

  // 为活跃 Run 开始新的 Attempt（开始、挂起后恢复、接管）。进程内的记忆只在同一个 Run 上保留。
  // Attempt 用尽时 Run 失败（Worker.startAttempt 的 MaxTakeovers，以当前 Attempt 写入）。
  action startAttempt(w: str): bool = {
    val r = active
    val wk = workers.get(w)
    val nextAttempt = curAttempt(r) + 1
    all {
      r > 0,
      if (nextAttempt <= MAX_ATTEMPTS) all {
        log' = log.append(ev("attempt", r, nextAttempt)),
        workers' = workers.set(w, { ...IDLE, attempt: nextAttempt, rid: r, phase: "ready", mem: if (wk.rid == r) wk.mem else EMPTY }),
      } else all {
        log' = log.append(ev("failed", r, curAttempt(r))),
        workers' = workers.set(w, { ...IDLE, rid: wk.rid, mem: wk.mem }),
      },
      through' = through,
      nextId' = nextId,
      ghost' = ghost,
    }
  }

  // 依据当前日志发出这一步的请求：状态是本 Run 最近一条已写入日志的回答的状态，新 Run 为空。
  // 被接管的 Worker 不知情时也会发出（它的回答在提交时被隔离）。
  action sendRequest(w: str): bool = {
    val wk = workers.get(w)
    val r = wk.rid
    val prev = lastAm(r)
    val st =
      if (MUTANT == "worker_memory_state") wk.mem
      else if (MUTANT == "cross_run_state") sessionState
      else prev.state
    val req: Req = {
      rid: r, attempt: wk.attempt, turn: turns(r) + 1, state: st,
      prevCalls: prev.calls, prevResults: prev.calls.intersect(results(r)),
    }
    all {
      wk.phase == "ready",
      status(r) == "running",
      MUTANT == "skip_pending" or pending(r) == Set(),
      turns(r) < MAX_TURNS,
      workers' = workers.set(w, { ...wk, phase: "waiting", req: req }),
      ghost' = { ...ghost, requests: ghost.requests.append(req) },
      log' = log,
      through' = through,
      nextId' = nextId,
    }
  }

  // 外部服务回答：n 个调用与一个新的状态令牌，或者不合法。
  action answer(w: str, valid: bool, n: int): bool = {
    val wk = workers.get(w)
    // 不用 nextId.to(...)：Apalache 只接受常量边界的区间。
    val calls = if (n == 0) Set() else if (n == 1) Set(nextId) else Set(nextId, nextId + 1)
    val resp: Resp = { valid: valid, calls: calls, state: nextId + n }
    all {
      wk.phase == "waiting",
      workers' = workers.set(w, { ...wk, phase: "got", resp: resp, mem: if (valid) resp.state else wk.mem }),
      nextId' = nextId + n + 1,
      log' = log,
      through' = through,
      ghost' = ghost,
    }
  }

  // 不合法的回答不写日志，与模型错误同口径计数；同一 Attempt 连续达到上限时 Run 失败（失败的提交同样以 Attempt 隔离）。
  action invalidAnswer(w: str): bool = {
    val wk = workers.get(w)
    val errs = wk.errors + 1
    if (errs < MAX_ERRORS) all {
      workers' = workers.set(w, { ...wk, phase: "ready", errors: errs }),
      log' = log,
      through' = through,
      nextId' = nextId,
      ghost' = ghost,
    } else all {
      log' = if (owns(w)) log.append(ev("failed", wk.rid, wk.attempt)) else log,
      workers' = workers.set(w, { ...IDLE, rid: wk.rid, mem: wk.mem }),
      through' = through,
      nextId' = nextId,
      ghost' = ghost,
    }
  }

  // 审核不可用：回答丢弃、Run 挂起，之后由新的 Attempt 恢复并重新请求（ADR-0054）。
  action moderationUnavailable(w: str): bool = {
    val wk = workers.get(w)
    all {
      log' = if (owns(w)) log.append(ev("suspended", wk.rid, wk.attempt)) else log,
      workers' = workers.set(w, { ...IDLE, rid: wk.rid, mem: wk.mem }),
      through' = through,
      nextId' = nextId,
      ghost' = ghost,
    }
  }

  // 拦截：以拒答替换回复、丢弃调用，状态不写入，Run 结束（ADR-0062 第 5 条）。
  // 变体 block_keeps_step 保留状态与调用，让外部服务在被拦截的那步上继续。
  action moderationBlocked(w: str): bool = {
    val wk = workers.get(w)
    val r = wk.rid
    val keep = MUTANT == "block_keeps_step"
    val am: Ev = {
      ...ev("am", r, wk.attempt),
      turn: turns(r) + 1, reqTurn: wk.req.turn, reqState: wk.req.state,
      state: if (keep) wk.resp.state else EMPTY, calls: if (keep) wk.resp.calls else Set(),
      blocked: true, valid: wk.resp.valid,
    }
    val ends = am.calls == Set()
    all {
      log' =
        if (not(owns(w))) log
        else if (ends) log.append(am).append(ev("completed", r, wk.attempt))
        else log.append(am),
      workers' = workers.set(w, if (owns(w) and not(ends)) { ...wk, phase: "ready", errors: 0 } else { ...IDLE, rid: r, mem: wk.mem }),
      through' = through,
      nextId' = nextId,
      ghost' = if (owns(w)) { ...ghost, seen: ghost.seen.union(Set("blocked")) } else ghost,
    }
  }

  // 写入回答：仍持有 Attempt 时追加 AssistantMessage（状态随它写入），没有调用时 Run 随即完成；否则放弃。
  // 变体 no_fencing 只看 Run 是否终态；变体 execute_unlogged 在写入之前就执行回答中的调用。
  action commitAnswer(w: str): bool = {
    val wk = workers.get(w)
    val r = wk.rid
    val ok = if (MUTANT == "no_fencing") r > 0 and not(terminal(r)) else owns(w)
    val am: Ev = {
      ...ev("am", r, wk.attempt),
      turn: turns(r) + 1, reqTurn: wk.req.turn, reqState: wk.req.state,
      state: wk.resp.state, calls: wk.resp.calls, valid: wk.resp.valid,
    }
    val done = am.calls == Set()
    val marks = Set(
      if (not(ok) and turns(r) >= wk.req.turn) "staleRefused" else "",
      if (ok and wk.errors > 0) "retriedAfterInvalid" else "",
      if (ok and runEvents(r).select(e => e.kind == "suspended").length() > 0) "resumedAfterSuspend" else "",
      if (ok and wk.req.prevCalls.size() == 2) "afterParallel" else "",
    )
    all {
      log' =
        if (not(ok)) log
        else if (done) log.append(am).append(ev("completed", r, wk.attempt))
        else log.append(am),
      workers' = workers.set(w, if (ok and not(done)) { ...wk, phase: "ready", errors: 0 } else { ...IDLE, rid: r, mem: wk.mem }),
      through' = through,
      nextId' = nextId,
      ghost' = {
        ...ghost,
        executed: if (MUTANT == "execute_unlogged") ghost.executed.union(wk.resp.calls) else ghost.executed,
        seen: ghost.seen.union(marks),
      },
    }
  }

  // 处理收到的回答：先校验，再送检，再写入。
  action handle(w: str, verdict: str): bool = all {
    workers.get(w).phase == "got",
    if (not(workers.get(w).resp.valid) and MUTANT != "log_invalid") invalidAnswer(w)
    else if (verdict == "unavailable") moderationUnavailable(w)
    else if (verdict == "block") moderationBlocked(w)
    else commitAnswer(w),
  }

  // 持有 Attempt 的 Worker 执行上一条回答中的下一个调用并写入结果。
  action execute(w: str): bool = {
    val wk = workers.get(w)
    val r = wk.rid
    val p = pending(r)
    val c = p.fold(0, (a, x) => if (a == 0 or x < a) x else a)
    all {
      wk.phase == "ready",
      owns(w),
      p != Set(),
      log' = log.append({ ...ev("result", r, wk.attempt), call: c }),
      ghost' = { ...ghost, executed: ghost.executed.union(Set(c)) },
      workers' = workers,
      through' = through,
      nextId' = nextId,
    }
  }

  // 轮次用尽时 Run 失败（AgentDef 的 max_turns）。
  action exceedTurns(w: str): bool = {
    val wk = workers.get(w)
    all {
      wk.phase == "ready",
      owns(w),
      pending(wk.rid) == Set(),
      turns(wk.rid) >= MAX_TURNS,
      log' = log.append(ev("failed", wk.rid, wk.attempt)),
      workers' = workers.set(w, { ...IDLE, rid: wk.rid, mem: wk.mem }),
      through' = through,
      nextId' = nextId,
      ghost' = ghost,
    }
  }

  // 进程崩溃：在途的请求与记忆一并丢失，持有的 Attempt 留待接管。
  action crash(w: str): bool = all {
    workers.get(w).phase != "idle",
    workers' = workers.set(w, IDLE),
    log' = log,
    through' = through,
    nextId' = nextId,
    ghost' = ghost,
  }

  // 两步之间的输入：插话、迟到的后台结果与取消、通话转写。
  action steer: bool = all {
    active > 0,
    inputs < MAX_INPUTS,
    log' = log.append(ev("input", active, 0)),
    through' = through,
    workers' = workers,
    nextId' = nextId,
    ghost' = ghost,
  }

  // 上一个 Run 终态后的新输入开启新的 Run（重答同样从空状态开始）。
  action newRun: bool = all {
    numRuns < MAX_RUNS,
    terminal(numRuns),
    log' = log.append(ev("requested", numRuns + 1, 0)),
    through' = through,
    workers' = workers,
    nextId' = nextId,
    ghost' = ghost,
  }

  action compactAt(i: int): bool = {
    val r = active
    val hasTail = lastAmIdx(r) >= 0 and lastAmIdx(r) < log.length() - 2
    all {
      canCompact(i),
      through' = i,
      ghost' = {
        ...ghost,
        compactions: ghost.compactions + 1,
        seen: if (hasTail) ghost.seen.union(Set("compactedWithTail")) else ghost.seen,
      },
      log' = log,
      workers' = workers,
      nextId' = nextId,
    }
  }

  // pickCut 优先最晚的可行截断点（预算内保留 keep_recent，force 时保留为 0）：随机下标很少落在最近一条回答之后的窄窗口里。
  val latestCut: int = log.indices().fold(-1, (m, i) => if (i > m and canCompact(i)) i else m)

  action stutter: bool = all {
    log' = log,
    through' = through,
    workers' = workers,
    nextId' = nextId,
    ghost' = ghost,
  }

  // Quint 在启用的分支中均匀选择：Worker 的动作各占一个分支，环境（插话、新 Run、压缩）合计三个；
  // 只有全部 Run 终态、Worker 都空闲时才原地不动（否则这一分支挤占步数）。
  action step = any {
    { nondet w = WORKERS.oneOf() startAttempt(w) },
    { nondet w = WORKERS.oneOf() sendRequest(w) },
    { nondet w = WORKERS.oneOf() nondet valid = Set(true, false).oneOf() nondet n = 0.to(MAX_CALLS).oneOf() answer(w, valid, n) },
    { nondet w = WORKERS.oneOf() nondet verdict = Set("ok", "unavailable", "block").oneOf() handle(w, verdict) },
    { nondet w = WORKERS.oneOf() execute(w) },
    { nondet w = WORKERS.oneOf() exceedTurns(w) },
    { nondet w = WORKERS.oneOf() crash(w) },
    steer,
    newRun,
    {
      nondet i = log.indices().oneOf()
      nondet latest = Set(true, false).oneOf()
      if (latest) all { latestCut >= 0, compactAt(latestCut) } else compactAt(i)
    },
    all { active == 0, numRuns == MAX_RUNS, WORKERS.forall(w => workers.get(w).phase == "idle"), stutter },
  }

  // ---- 安全性质 ----

  // 状态链：每个 Run 第一步的请求状态为空（新 Run、重答），之后每一步带回本 Run 上一条已写入日志的回答的状态。
  // 写入前崩溃、接管、审核等待与不合法的回答都不改变它（ADR-0062 第 2、3 条）。
  val invStateChain: bool = RUNS.forall(r => {
    val a = ams(r)
    a.indices().forall(i => a[i].reqState == (if (i == 0) EMPTY else a[i - 1].state))
  })

  def attemptBefore(r: int, i: int): int = log.slice(0, i).foldl(0, (a, e) => if (e.kind == "attempt" and e.rid == r) e.attempt else a)

  // 回答、结果与 Run 的状态变化都由写入时持有 Attempt 的 Worker 写入：旧 Attempt 迟到的回答不写入。
  val invFenced: bool = log.indices().forall(i =>
    Set("am", "result", "completed", "failed", "suspended").contains(log[i].kind)
      implies log[i].attempt == attemptBefore(log[i].rid, i))

  // 同一轮只写入一条回答，且正是为这一轮发出的请求的回答。
  val invOneAnswerPerTurn: bool = RUNS.forall(r => {
    val a = ams(r)
    a.indices().forall(i => a[i].turn == i + 1 and a[i].reqTurn == i + 1)
  })

  // 平台只执行已写入日志的回答中的调用（回答写入日志后才生效，ADR-0041 第 3 条）。
  val invActsOnlyLogged: bool =
    ghost.executed.subseteq(log.foldl(Set(), (s, e) => if (e.kind == "am") s.union(e.calls) else s))

  // 每次请求的"上一步"：上一条回答的每个调用都恰有一条结果（ADR-0062 第 5 条）。
  val invPairing: bool = ghost.requests.indices().forall(i => ghost.requests[i].prevResults == ghost.requests[i].prevCalls)

  // 不合法的回答不写日志（ADR-0062 第 4 条）。
  val invOnlyValidLogged: bool = log.foldl(true, (ok, e) => ok and (e.kind != "am" or e.valid))

  // 被拦截的回答没有调用与状态，是这个 Run 的最后一条回答，Run 随之结束（ADR-0062 第 5 条）。
  val invBlockedEnds: bool = RUNS.forall(r => {
    val a = ams(r)
    a.indices().forall(i => a[i].blocked implies and {
      i == a.length() - 1,
      a[i].calls == Set(),
      a[i].state == EMPTY,
      status(r) == "completed",
    })
  })

  // 由外部循环驱动的活跃 Run：最近一条回答保留原文，没有进入摘要（ADR-0062 第 5 条，新增）。
  val invLatestStepRaw: bool = (active > 0 and lastAmIdx(active) >= 0) implies through < lastAmIdx(active)

  val allInvariants: bool = and {
    invStateChain,
    invFenced,
    invOneAnswerPerTurn,
    invActsOnlyLogged,
    invPairing,
    invOnlyValidLogged,
    invBlockedEnds,
    invLatestStepRaw,
  }

  // ---- 可达性见证：每个都应被随机模拟违反，证明模拟走到了对应的状态 ----

  def repeated(a: Req, b: Req): bool = a.rid == b.rid and a.turn == b.turn and a.attempt < b.attempt and turns(a.rid) >= a.turn

  // 同一轮被不同 Attempt 请求过，且这一轮已写入日志。
  val witnessNoRepeatedStep: bool = not(ghost.requests.indices().exists(i =>
    ghost.requests.indices().exists(j => repeated(ghost.requests[i], ghost.requests[j]))))

  // 旧 Attempt 的回答在这一轮已被写入之后到达、被放弃。
  val witnessNoStaleRefused: bool = not(ghost.seen.contains("staleRefused"))

  // 同一 Attempt 中不合法的回答之后重新请求并写入。
  val witnessNoRetryAfterInvalid: bool = not(ghost.seen.contains("retriedAfterInvalid"))

  // 审核不可用挂起后恢复并写入。
  val witnessNoResumeAfterSuspend: bool = not(ghost.seen.contains("resumedAfterSuspend"))

  // 回答被拦截。
  val witnessNoBlocked: bool = not(ghost.seen.contains("blocked"))

  // 同一步两个调用的结果都交回后写入下一步。
  val witnessNoAfterParallel: bool = not(ghost.seen.contains("afterParallel"))

  // 最近一条回答之后还有事件（结果、插话）时压缩。
  val witnessNoCompactedWithTail: bool = not(ghost.seen.contains("compactedWithTail"))

  // 第一个 Run 留下非空状态后，第二个 Run 写入了回答。
  val witnessNoSecondRunStep: bool = not(numRuns == 2 and turns(1) >= 1 and turns(2) >= 1 and lastAm(1).state != EMPTY)
}

module correct { import external_loop(MUTANT = "none").* }

module scenarios {
  import external_loop(MUTANT = "none").*

  // 接管：w1 的请求在途时 w2 接管并写入这一轮；w1 迟到的回答被放弃。两次请求的轮次与状态相同。
  run takeoverRefusesStaleAnswerTest =
    init
      .then(startAttempt("w1")).then(sendRequest("w1")).then(answer("w1", true, 1))
      .then(startAttempt("w2")).then(sendRequest("w2")).then(answer("w2", true, 1)).then(handle("w2", "ok"))
      .then(all {
        assert(workers.get("w1").req.turn == workers.get("w2").req.turn),
        assert(workers.get("w1").req.state == workers.get("w2").req.state),
        handle("w1", "ok"),
      })
      .then(all {
        assert(turns(1) == 1),
        assert(lastAm(1).attempt == 2),
        assert(ghost.seen.contains("staleRefused")),
        assert(allInvariants),
        stutter,
      })

  // 状态链经过审核等待与接管：挂起时丢弃的回答不写入，恢复后的请求带回最近一条写入日志的状态。
  run stateSurvivesSuspendAndTakeoverTest =
    init
      .then(startAttempt("w1")).then(sendRequest("w1")).then(answer("w1", true, 1)).then(handle("w1", "ok"))
      .then(execute("w1")).then(sendRequest("w1")).then(answer("w1", true, 0)).then(handle("w1", "unavailable"))
      .then(all {
        assert(status(1) == "suspended"),
        assert(turns(1) == 1),
        startAttempt("w2"),
      })
      .then(sendRequest("w2"))
      .then(all {
        assert(workers.get("w2").req.state == lastAm(1).state),
        assert(workers.get("w2").req.state != EMPTY),
        assert(workers.get("w2").req.prevResults == lastAm(1).calls),
        answer("w2", true, 0),
      })
      .then(handle("w2", "ok"))
      .then(all {
        assert(status(1) == "completed"),
        assert(allInvariants),
        stutter,
      })

  // 拦截结束 Run、状态不写入；新 Run 的第一步状态为空。
  run blockedEndsRunAndNewRunStartsEmptyTest =
    init
      .then(startAttempt("w1")).then(sendRequest("w1")).then(answer("w1", true, 1)).then(handle("w1", "ok"))
      .then(execute("w1")).then(sendRequest("w1")).then(answer("w1", true, 1)).then(handle("w1", "block"))
      .then(all {
        assert(status(1) == "completed"),
        assert(lastAm(1).blocked and lastAm(1).calls == Set() and lastAm(1).state == EMPTY),
        newRun,
      })
      .then(startAttempt("w2")).then(sendRequest("w2"))
      .then(all {
        assert(workers.get("w2").req.state == EMPTY),
        assert(allInvariants),
        stutter,
      })

  // 最近一条回答之后有结果与插话时，内置规则允许截在结果处（最近一步进入摘要），外部循环的规则不允许；
  // 截在最近一条回答之前仍可以。
  run compactionKeepsLatestStepTest =
    init
      .then(startAttempt("w1")).then(sendRequest("w1")).then(answer("w1", true, 1)).then(handle("w1", "ok"))
      .then(execute("w1")).then(steer)
      .then(all {
        // log：requested、attempt、am、result、input
        assert(lastAmIdx(1) == 2),
        assert(not(canCompact(3))),
        assert(canCompact(1)),
        compactAt(1),
      })
      .then(all {
        assert(through == 1),
        assert(allInvariants),
        stutter,
      })
}

// 变体：各违反 ADR-0062 的一处。
// 提交不以 Attempt 隔离：旧 Attempt 迟到的回答也写入。
module no_fencing { import external_loop(MUTANT = "no_fencing").* }
// Worker 带上进程内记住的状态，而不是日志中的：接管或崩溃后状态链断开。
module worker_memory_state { import external_loop(MUTANT = "worker_memory_state").* }
// 新 Run 带上整个 Session 最近一条回答的状态。
module cross_run_state { import external_loop(MUTANT = "cross_run_state").* }
// 写入之前就执行回答中的调用：写入被放弃时，执行过的调用不在日志中。
module execute_unlogged { import external_loop(MUTANT = "execute_unlogged").* }
// 上一步的调用还没有结果就发出下一步的请求。
module skip_pending { import external_loop(MUTANT = "skip_pending").* }
// 不合法的回答也写入日志。
module log_invalid { import external_loop(MUTANT = "log_invalid").* }
// 拦截后保留状态与调用，外部服务在被拦截的那步上继续。
module block_keeps_step { import external_loop(MUTANT = "block_keeps_step").* }
// 只用内置循环的截断规则：最近一步之后有结果与插话时，最近一步进入摘要。
module builtin_compaction { import external_loop(MUTANT = "builtin_compaction").* }
