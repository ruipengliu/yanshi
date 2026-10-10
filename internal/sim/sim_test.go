package sim

import (
	"flag"
	"reflect"
	"strings"
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
		t.Fatalf("seed %d: %v\nreproduce: go test ./internal/sim -run %s -sim.seed=%d\nlast steps:\n%s",
			s, err, t.Name(), s, strings.Join(tail, "\n"))
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
	var total Stats
	for _, s := range seedList() {
		total.add(run(t, s, Options{Faults: true}).Stats)
	}
	requireCoverage(t, total, "Runs", "Steers", "Interrupts", "Crashes", "Takeovers", "OutcomeUnknown", "Retried",
		"NodeCrashes", "Redeliveries", "Approvals", "Denials", "Suspensions", "DeviceResults", "Timeouts",
		"ControllerCrashes", "SandboxExecs", "Reaps", "EnqueueInterleavings", "Closes", "Deletions", "JanitorCrashes", "DeletionsCompleted", "Recalls", "MemorySaves", "Compactions", "SummaryFaults", "Overflows", "AttemptsAfterCompaction",
		"QuotaSuspensions", "QuotaRejections", "QuotaResumes", "AccountDeletions", "AgentSwitches",
		"InputBlocks", "OutputBlocks", "ModerationErrors", "MemoryApprovals", "PresenceWrites", "PresenceWithdrawn",
		"Questions", "Answers", "TypedAnswers", "InvalidAnswers", "QuestionTimeouts", "UIContextInputs")
}

// TestLongRunsWithFaults 让 Run 持续上百轮，检验压缩在故障下保持上下文有界、调用配对完整，且长 Run 能跑完。
func TestLongRunsWithFaults(t *testing.T) {
	var total Stats
	list := seedList()
	if *seed == 0 {
		list = list[:max(len(list)/5, 1)]
	}
	for _, s := range list {
		total.add(run(t, s, Options{Faults: true, LongRuns: true, Sessions: 2, Ticks: 1500}).Stats)
	}
	requireCoverage(t, total, "Compactions", "SummaryFaults", "Overflows", "AttemptsAfterCompaction", "Takeovers",
		"Suspensions", "LongRunsCompleted")
}

func TestSimulationWithoutFaultsNeverFails(t *testing.T) {
	for _, s := range seedList() {
		if st := run(t, s, Options{}).Stats; st.Failed != 0 || st.Takeovers != 0 || st.OutcomeUnknown != 0 {
			t.Fatalf("seed %d: fault-free run had failures/takeovers: %+v", s, st)
		}
	}
}

func TestSimulationIsDeterministic(t *testing.T) {
	for _, s := range []uint64{1, 7, 42} {
		a, b := run(t, s, Options{Faults: true, LongRuns: s == 7}), run(t, s, Options{Faults: true, LongRuns: s == 7})
		if a.Fingerprint() != b.Fingerprint() {
			t.Fatalf("seed %d produced different logs", s)
		}
	}
}
