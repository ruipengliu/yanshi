package sim

import (
	"flag"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

var (
	seeds = flag.Int("sim.seeds", 300, "number of seeds to simulate")
	seed  = flag.Uint64("sim.seed", 0, "run only this seed (0 = all)")
)

func run(t *testing.T, s uint64, opts Options) *World {
	t.Helper()
	opts.Seed = s
	if opts.Workers == 0 {
		opts.Workers = 3
	}
	if opts.Sessions == 0 {
		opts.Sessions = 3
	}
	if opts.Ticks == 0 {
		opts.Ticks = 400
	}
	w, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Run(); err != nil {
		tail := w.Trace
		if len(tail) > 40 {
			tail = tail[len(tail)-40:]
		}
		// 复现命令用顶层测试名：种子在分组子测试中运行，而 -sim.seed 只剩一个种子时分组不同，子测试名对不上。
		top, _, _ := strings.Cut(t.Name(), "/")
		t.Fatalf("seed %d: %v\nreproduce: go test ./internal/sim -run '^%s$' -sim.seed=%d\nlast steps:\n%s",
			s, err, top, s, strings.Join(tail, "\n"))
	}
	w.CollectStats()
	return w
}

func seedList() []uint64 {
	if *seed != 0 {
		return []uint64{*seed}
	}
	out := make([]uint64, *seeds)
	for i := range out {
		out[i] = uint64(i + 1)
	}
	return out
}

// parallelSeeds 并行模拟 list 中的种子，返回各种子 Stats 之和；任一种子失败时 t 立即失败。
//
// World 之间不共享可变状态，每个种子仍由一个 goroutine 确定性地单步推进，并行只缩短墙钟时间，不改变任何种子的结果。
// 种子分给 GOMAXPROCS 个并行的分组子测试，各组从同一个计数器领取下一个种子，长短种子因此均摊到各组。
// 分组外再套一层不并行的子测试：它在所有组结束后才返回，调用方的覆盖统计因此在全部种子跑完后计算。
func parallelSeeds(t *testing.T, list []uint64, each func(t *testing.T, s uint64) Stats) Stats {
	t.Helper()
	var (
		mu    sync.Mutex
		total Stats
		next  atomic.Int64
		// failed 使其余组在有种子失败后跑完手头的种子即停，接近串行时"首个失败即停"，失败输出不被淹没。
		failed atomic.Bool
	)
	groups := min(runtime.GOMAXPROCS(0), len(list))
	ok := t.Run("seeds", func(t *testing.T) {
		for g := range groups {
			t.Run(fmt.Sprintf("g%d", g), func(t *testing.T) {
				t.Parallel()
				defer func() {
					if t.Failed() {
						failed.Store(true)
					}
				}()
				var sum Stats
				for !failed.Load() {
					i := int(next.Add(1)) - 1
					if i >= len(list) {
						break
					}
					sum.add(each(t, list[i]))
				}
				mu.Lock()
				total.add(sum)
				mu.Unlock()
			})
		}
	})
	if !ok {
		// 有种子失败时其所在组的计数不完整，不再检查覆盖。
		t.FailNow()
	}
	return total
}

// add 把 o 的各项计数累加到 s。
func (s *Stats) add(o Stats) {
	a, b := reflect.ValueOf(s).Elem(), reflect.ValueOf(o)
	for i := range a.NumField() {
		a.Field(i).SetInt(a.Field(i).Int() + b.Field(i).Int())
	}
}

// requireCoverage 要求模拟真正触达 names 列出的路径，否则通过没有意义。
func requireCoverage(t *testing.T, total Stats, names ...string) {
	t.Helper()
	t.Logf("coverage: %+v", total)
	if *seed != 0 {
		return
	}
	v := reflect.ValueOf(total)
	for _, name := range names {
		if v.FieldByName(name).Int() == 0 {
			t.Errorf("simulation never exercised %s", name)
		}
	}
}

func TestSimulationWithFaults(t *testing.T) {
	total := parallelSeeds(t, seedList(), func(t *testing.T, s uint64) Stats {
		return run(t, s, Options{Faults: true}).Stats
	})
	requireCoverage(t, total, "Runs", "Steers", "Interrupts", "Crashes", "Takeovers", "OutcomeUnknown", "Retried",
		"NodeCrashes", "Redeliveries", "Approvals", "Denials", "Suspensions", "DeviceResults", "Timeouts",
		"ControllerCrashes", "SandboxExecs", "Reaps", "EnqueueInterleavings", "Closes", "Deletions", "JanitorCrashes", "DeletionsCompleted", "Recalls", "MemorySaves", "Compactions", "SummaryFaults", "Overflows", "AttemptsAfterCompaction",
		"QuotaSuspensions", "QuotaRejections", "QuotaResumes", "AccountDeletions", "AgentSwitches",
		"InputBlocks", "OutputBlocks", "ModerationErrors", "MemoryApprovals", "PresenceWrites", "PresenceWithdrawn",
		"Questions", "Answers", "TypedAnswers", "InvalidAnswers", "QuestionTimeouts", "UIContextInputs", "DuplicateInputs",
		"Cards", "CardActions", "CardDuplicates", "CardAnswers",
		"Notifications", "NotificationsWatching", "PushFailures",
		"CallsStarted", "CallsReplaced", "CallTranscripts", "CallTranscriptsBlocked", "CallTasks", "CallAnswers", "CallConflicts",
		"StoreFaults", "WakeFaults", "UncertainRetries", "CatalogFaults", "ResumedClaims", "PastRuns", "Delegations", "DelegationResults", "ChildDeletionRaces", "CascadeFaults", "Decisions", "DecisionFallbacks", "GracefulStops", "Handoffs",
		"Backgrounded", "BackgroundResults", "BackgroundTimeouts", "BackgroundCancels", "DisconnectWakes")
}

// TestLongRunsWithFaults 让 Run 持续上百轮，检验压缩在故障下保持上下文有界、调用配对完整，且长 Run 能跑完。
func TestLongRunsWithFaults(t *testing.T) {
	list := seedList()
	if *seed == 0 {
		list = list[:max(len(list)/5, 1)]
	}
	total := parallelSeeds(t, list, func(t *testing.T, s uint64) Stats {
		return run(t, s, Options{Faults: true, LongRuns: true, Sessions: 2, Ticks: 1500}).Stats
	})
	requireCoverage(t, total, "Compactions", "SummaryFaults", "Overflows", "AttemptsAfterCompaction", "Takeovers",
		"Suspensions", "LongRunsCompleted", "Handoffs")
}

// TestSimulationWithoutFaultsNeverFails：没有故障时 Run 不失败、不被接管。没有故障就没有停机，移交都来自交互 Worker
// 移交长任务：它正常结束 Attempt，不算接管（ADR-0029）。
func TestSimulationWithoutFaultsNeverFails(t *testing.T) {
	total := parallelSeeds(t, seedList(), func(t *testing.T, s uint64) Stats {
		st := run(t, s, Options{}).Stats
		if st.Failed != 0 || st.Takeovers != 0 || st.OutcomeUnknown != 0 {
			t.Fatalf("seed %d: fault-free run had failures/takeovers: %+v", s, st)
		}
		return st
	})
	requireCoverage(t, total, "Handoffs")
}

// TestSimulationIsDeterministic：同一种子跑两遍，日志逐字节相同。各种子的两份 World 同时运行：并行时结果也不能变，
// 且 make check 以 -race 运行本测试，竞争检测器借此检查 World 之间是否共享了可变状态。
func TestSimulationIsDeterministic(t *testing.T) {
	seeds := []uint64{1, 7, 42}
	prints := make([][2]string, len(seeds))
	ok := t.Run("worlds", func(t *testing.T) {
		for i, s := range seeds {
			for j := range prints[i] {
				t.Run(fmt.Sprintf("seed%d-%d", s, j), func(t *testing.T) {
					t.Parallel()
					prints[i][j] = run(t, s, Options{Faults: true, LongRuns: s == 7}).Fingerprint()
				})
			}
		}
	})
	if !ok {
		t.FailNow()
	}
	for i, s := range seeds {
		if prints[i][0] != prints[i][1] {
			t.Fatalf("seed %d produced different logs", s)
		}
	}
}
