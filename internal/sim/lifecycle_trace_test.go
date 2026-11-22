package sim

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"yanshi/internal/agentdef"
	"yanshi/internal/app"
	"yanshi/internal/artifact"
	"yanshi/internal/clock"
	"yanshi/internal/eventlog"
	"yanshi/internal/ids"
	"yanshi/internal/lifecycle"
	"yanshi/internal/memory"
	"yanshi/internal/model"
	"yanshi/internal/presence"
	"yanshi/internal/sandbox"
	"yanshi/internal/service"
)

// traceObs 记录每一步操作结束后，从真实 Go 存储中观测到的关键状态快照，
// 用于在导出的 Quint 回放测试中逐帧 assert 规约状态与真实实现一致。
type traceObs struct {
	action       string
	tombstone    string
	completed    bool
	hasLog       bool
	hasIndex     bool
	data         []string
	memories     []string
	janitorStage string
}

type traceEnv struct {
	t      *testing.T
	clk    *clock.Fake
	stores app.Stores
	core   *app.Core
	mem    *memory.MemStore
	pres   *presence.Mem
	sid    string

	ordToConn map[string]string // Quint ord ID ("d1"/"d2") -> presence ConnID
	memToCall map[string]string // Quint mem ID ("m1"/"m2") -> Memory CallID
	steps     []traceObs
}

func newTraceEnv(t *testing.T) *traceEnv {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC))
	agents, err := agentdef.NewRegistry(&agentdef.Def{Name: "sim", Version: "1", Model: "echo/any"})
	if err != nil {
		t.Fatal(err)
	}
	stores := app.Memory(clk)
	core := app.NewCore(stores, app.Options{
		Clock:  clk,
		IDs:    ids.Sequential,
		Agents: agents,
		Model:  model.NewGateway(),
		Blobs:  artifact.NewMemBlobs(),
	})
	sid, err := core.Service.Create(context.Background(), service.CreateRequest{
		BusinessLine: "bl",
		EndUser:      "u",
		Agent:        "sim",
	})
	if err != nil {
		t.Fatal(err)
	}
	return &traceEnv{
		t:         t,
		clk:       clk,
		stores:    stores,
		core:      core,
		mem:       stores.Memories.(*memory.MemStore),
		pres:      stores.Presence.(*presence.Mem),
		sid:       sid,
		ordToConn: map[string]string{"d1": "conn-d1", "d2": "conn-d2"},
		memToCall: map[string]string{"m1": "call-m1", "m2": "call-m2"},
	}
}

// snapshot 从真实内存存储中读取当前状态，并与 Quint 抽象变量同构化后记录。
func (e *traceEnv) snapshot(action, janitorStage string) traceObs {
	e.t.Helper()
	ctx := context.Background()

	tombName := "none"
	completed := false
	if tomb, err := e.stores.Deletions.Get(ctx, e.sid); err == nil {
		tombName = tomb.Reason
		completed = !tomb.CompletedAt.IsZero()
	} else if !errors.Is(err, lifecycle.ErrNotFound) {
		e.t.Fatalf("deletions.Get: %v", err)
	}

	events, err := eventlog.ReadAll(ctx, e.stores.Log, e.sid, 0)
	if err != nil {
		e.t.Fatalf("eventlog.ReadAll: %v", err)
	}
	hasLog := len(events) > 0

	hasIndex := true
	if _, err := e.stores.Index.LookupSession(ctx, e.sid); errors.Is(err, lifecycle.ErrNotFound) {
		hasIndex = false
	} else if err != nil {
		e.t.Fatalf("index.LookupSession: %v", err)
	}

	entries, _, err := e.stores.Presence.List(ctx, e.sid, time.Time{})
	if err != nil {
		e.t.Fatalf("presence.List: %v", err)
	}
	connPresent := map[string]bool{}
	for _, en := range entries {
		connPresent[en.ConnID] = true
	}
	var data []string
	for _, id := range []string{"d1", "d2"} {
		if connPresent[e.ordToConn[id]] {
			data = append(data, id)
		}
	}

	var memories []string
	for _, id := range []string{"m1", "m2"} {
		mid, err := memory.CallMemoryID(memory.SaveRequest{
			BusinessLine: "bl",
			EndUser:      "u",
			SessionID:    e.sid,
			CallID:       e.memToCall[id],
		})
		if err != nil {
			e.t.Fatalf("CallMemoryID: %v", err)
		}
		if _, err := e.stores.Memories.Get(ctx, "u", mid); err == nil {
			memories = append(memories, id)
		} else if !errors.Is(err, memory.ErrNotFound) {
			e.t.Fatalf("memories.Get: %v", err)
		}
	}

	return traceObs{
		action:       action,
		tombstone:    tombName,
		completed:    completed,
		hasLog:       hasLog,
		hasIndex:     hasIndex,
		data:         data,
		memories:     memories,
		janitorStage: janitorStage,
	}
}

func (e *traceEnv) record(action, janitorStage string) {
	e.steps = append(e.steps, e.snapshot(action, janitorStage))
}

// writePresence 驱动真实的 presence.Service.Put（内部完成 Put -> lifecycle.Recheck -> DeleteSession 撤回）。
// 为使轨迹与 Quint 中的细粒度动作对应，将 Put 拆为 ordWrite -> ordRecheck (-> ordWithdraw)。
func (e *traceEnv) writePresence(id string) {
	e.t.Helper()
	ctx := context.Background()
	svc := &presence.Service{
		Store:     e.stores.Presence,
		Deletions: e.stores.Deletions,
		Clock:     e.clk,
	}
	goneBefore, err := lifecycle.Deleted(ctx, e.stores.Deletions, e.sid)
	if err != nil {
		e.t.Fatal(err)
	}

	// 直接调用真实 presence.Service.Put：其内部顺序为 Store.Put -> lifecycle.Recheck(DeleteSession)。
	if err := svc.Put(ctx, &presence.Entry{
		SessionID: e.sid,
		ConnID:    e.ordToConn[id],
		DeviceID:  "dev-1",
		Expires:   e.clk.Now().Add(time.Minute),
	}); err != nil {
		e.t.Fatalf("presence.Put(%s): %v", id, err)
	}

	if goneBefore {
		// 发生撤回时：在最终观测为已撤回前，先映射 ordWrite -> ordRecheck -> ordWithdraw 三个原子步；
		// 仅在最后一步后校验真实存储已被 Recheck 彻底撤回（不存在该条记录）。
		e.steps = append(e.steps,
			traceObs{action: fmt.Sprintf(`ordWrite("%s")`, id), tombstone: "none", janitorStage: "SKIP_ASSERT"},
			traceObs{action: fmt.Sprintf(`ordRecheck("%s")`, id), tombstone: "none", janitorStage: "SKIP_ASSERT"},
		)
		e.record(fmt.Sprintf(`ordWithdraw("%s")`, id), "idle")
		if e.pres.HasSession(e.sid) {
			e.t.Fatalf("presence of deleted session %s survived Recheck", e.sid)
		}
		return
	}

	e.steps = append(e.steps,
		traceObs{action: fmt.Sprintf(`ordWrite("%s")`, id), tombstone: "none", janitorStage: "SKIP_ASSERT"},
	)
	e.record(fmt.Sprintf(`ordRecheck("%s")`, id), "idle")
}

// writeMemory 驱动真实的 memory.Service.Save（内部完成 Store.Put -> lifecycle.Recheck -> Store.Delete 撤回）。
func (e *traceEnv) writeMemory(id string) {
	e.t.Helper()
	ctx := context.Background()
	goneBefore, err := lifecycle.Deleted(ctx, e.stores.Deletions, e.sid)
	if err != nil {
		e.t.Fatal(err)
	}

	_, err = e.core.Memory.Save(ctx, memory.SaveRequest{
		BusinessLine: "bl",
		EndUser:      "u",
		SessionID:    e.sid,
		CallID:       e.memToCall[id],
		Category:     "preference",
		Content:      "测试偏好 " + id,
	})
	if goneBefore {
		if !errors.Is(err, memory.ErrNotFound) {
			e.t.Fatalf("late memory.Save(%s) = %v, want ErrNotFound", id, err)
		}
		e.steps = append(e.steps,
			traceObs{action: fmt.Sprintf(`memWrite("%s")`, id), tombstone: "none", janitorStage: "SKIP_ASSERT"},
			traceObs{action: fmt.Sprintf(`memRecheck("%s")`, id), tombstone: "none", janitorStage: "SKIP_ASSERT"},
		)
		e.record(fmt.Sprintf(`memWithdraw("%s")`, id), "idle")
		return
	}
	if err != nil {
		e.t.Fatalf("memory.Save(%s): %v", id, err)
	}
	e.steps = append(e.steps,
		traceObs{action: fmt.Sprintf(`memWrite("%s")`, id), tombstone: "none", janitorStage: "SKIP_ASSERT"},
	)
	e.record(fmt.Sprintf(`memRecheck("%s")`, id), "idle")
}

func (e *traceEnv) deleteSession(reason string) {
	e.t.Helper()
	if err := e.core.Service.Delete(context.Background(), e.sid, reason, ""); err != nil {
		e.t.Fatalf("service.Delete(%s): %v", reason, err)
	}
	e.record(fmt.Sprintf(`markDeleted("%s")`, reason), "idle")
}

// runJanitor 运行真实 janitor.Janitor.Step；若 crashAtStage 非空，在进入该阶段时通过 BeforeStage 取消 ctx 模拟崩溃。
// hookMidLog 若非 nil，则在首次进入 delete_log 前先注入并发动作（例如模拟“删日志前或两遍清扫间的迟到写入”）。
func (e *traceEnv) runJanitor(id string, crashAtStage string, onCrashAfterLateWrite func()) bool {
	e.t.Helper()
	j := e.core.Janitor(id, sandbox.NewFake(), nil, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	crashed := false
	// 利用 Janitor.BeforeStage 钩子把真实阶段名映射到 Quint 动作，并同步断言存储状态。
	// janitor.go 执行顺序：interrupt -> forget -> children -> withdraw(sweep1) -> log -> sweep(sweep2) -> index delete & complete
	j.BeforeStage = func(_ context.Context, stage string) {
		switch stage {
		case "interrupt":
			e.record("janitorClaim", "interrupt")
			if crashAtStage == "interrupt" {
				crashed = true
				cancel()
			}
		case "withdraw":
			// interrupt / forget / children 合并映射为进入 sweep1
			e.record("janitorInterrupt", "sweep1")
			if crashAtStage == "withdraw" {
				crashed = true
				cancel()
			}
		case "log":
			// withdraw(sweep1) 刚刚完成，准备删除 log
			e.record("janitorSweep1", "delete_log")
			if crashAtStage == "log" {
				crashed = true
				cancel()
			}
		case "sweep":
			// log 与 snapshot 刚刚删除完成，准备第二遍清扫
			e.record("janitorDeleteLog", "sweep2")
			if crashAtStage == "sweep" {
				crashed = true
				cancel()
			}
		}
	}

	did, err := j.Step(ctx)
	if crashed {
		if err == nil {
			e.t.Fatalf("expected janitor %s to abort at stage %s", id, crashAtStage)
		}
		e.record("janitorCrash", "idle")
		if onCrashAfterLateWrite != nil {
			onCrashAfterLateWrite()
		}
		// 模拟崩溃后的租约过期或 Sweep 重新兜底入队（Janitor.LeaseTTL 默认 30s）。
		e.clk.Advance(35 * time.Second)
		return true
	}
	if err != nil {
		e.t.Fatalf("janitor.Step(%s): %v", id, err)
	}
	if !did {
		e.t.Fatalf("janitor.Step(%s) reported nothing to do", id)
	}
	// Step 成功返回意味着 sweep2、index 删除与 Deletions.Complete 均已完成。
	e.steps = append(e.steps, traceObs{action: "janitorSweep2", tombstone: "none", janitorStage: "SKIP_ASSERT"})
	e.record("janitorComplete", "idle")
	return false
}

// TestLifecycleRecheckTraceValidation 驱动真实 Go 核心服务与 Janitor 完成四条删查交错轨迹，
// 验证 Go 实现不变量；当设置环境变量 YANSHI_TRACE_OUT 时导出 Quint 回放测试文件做 Trace Validation。
func TestLifecycleRecheckTraceValidation(t *testing.T) {
	type scenario struct {
		name string
		run  func(*testing.T) *traceEnv
	}

	scenarios := []scenario{
		{
			name: "traceHardDeleteWithLateWrites",
			run: func(t *testing.T) *traceEnv {
				e := newTraceEnv(t)
				e.writePresence("d1")
				e.writeMemory("m1")
				e.deleteSession("end_user")
				e.writePresence("d2")
				e.writeMemory("m2")
				e.runJanitor("j1", "", nil)
				return e
			},
		},
		{
			name: "traceErasureKeepsPreWithdrawsLate",
			run: func(t *testing.T) *traceEnv {
				e := newTraceEnv(t)
				e.writePresence("d1")
				e.writeMemory("m1")
				e.deleteSession(lifecycle.ReasonErasure)
				e.writeMemory("m2")
				e.runJanitor("j1", "", nil)
				if !e.mem.HasSession(e.sid) {
					t.Fatalf("erasure removed pre-deletion memory")
				}
				return e
			},
		},
		{
			name: "traceRetentionJanitorCrashRecovery",
			run: func(t *testing.T) *traceEnv {
				e := newTraceEnv(t)
				e.writePresence("d1")
				e.writeMemory("m1")
				e.deleteSession(lifecycle.ReasonRetention)
				if !e.runJanitor("j1-crash", "log", nil) {
					t.Fatal("expected crash at log stage")
				}
				e.runJanitor("j2-recover", "", nil)
				if !e.mem.HasSession(e.sid) {
					t.Fatalf("retention removed pre-deletion memory")
				}
				return e
			},
		},
		{
			name: "traceJanitorCrashAfterLogDeleteAndLateWrite",
			run: func(t *testing.T) *traceEnv {
				e := newTraceEnv(t)
				e.writePresence("d1")
				e.deleteSession("end_user")
				if !e.runJanitor("j1-crash", "sweep", func() {
					// 在删完日志、第二遍清扫前崩溃的窗口期落盘迟到写入，验证 Recheck 撤回与第二遍清扫兜底。
					e.writePresence("d2")
				}) {
					t.Fatal("expected crash at sweep stage")
				}
				e.runJanitor("j2-recover", "", nil)
				return e
			},
		},
	}

	var b strings.Builder
	b.WriteString("// Code generated by internal/sim/lifecycle_trace_test.go; DO NOT EDIT.\n")
	b.WriteString("module trace_replay_test {\n")
	b.WriteString("  import lifecycle_recheck.* from \"../lifecycle_recheck\"\n\n")

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			e := sc.run(t)
			last := e.steps[len(e.steps)-1]
			if !last.completed || last.hasLog || last.hasIndex || len(last.data) != 0 {
				t.Fatalf("final Go state not clean: %+v", last)
			}
			b.WriteString(renderQuintTraceRun(sc.name, e.steps))
		})
	}
	b.WriteString("}\n")

	if out := os.Getenv("YANSHI_TRACE_OUT"); out != "" {
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("exported Quint trace replay file to %s", out)
	}
}

func renderQuintTraceRun(name string, steps []traceObs) string {
	var b strings.Builder
	fmt.Fprintf(&b, "  run %sTest = {\n", name)
	b.WriteString("    init\n")
	for _, st := range steps {
		fmt.Fprintf(&b, "      .then(%s)\n", st.action)
		if st.janitorStage == "SKIP_ASSERT" {
			continue
		}
		fmt.Fprintf(&b, "      .then(all {\n")
		fmt.Fprintf(&b, "        assert(tombstone == %q),\n", st.tombstone)
		fmt.Fprintf(&b, "        assert(completed == %t),\n", st.completed)
		fmt.Fprintf(&b, "        assert(hasLog == %t),\n", st.hasLog)
		fmt.Fprintf(&b, "        assert(hasIndex == %t),\n", st.hasIndex)
		fmt.Fprintf(&b, "        assert(janitorStage == %q),\n", st.janitorStage)
		fmt.Fprintf(&b, "        assert(data == %s),\n", formatQuintSet(st.data))
		fmt.Fprintf(&b, "        assert(memories == %s),\n", formatQuintSet(st.memories))
		b.WriteString("        assert(allInvariants),\n")
		b.WriteString("        stutter,\n")
		b.WriteString("      })\n")
	}
	b.WriteString("  }\n\n")
	return b.String()
}

func formatQuintSet(items []string) string {
	if len(items) == 0 {
		return "Set()"
	}
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	return "Set(" + strings.Join(quoted, ", ") + ")"
}
