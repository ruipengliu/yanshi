package sim

import (
	"flag"
	"strings"
	"testing"
)

var (
	seeds = flag.Int("sim.seeds", 300, "number of seeds to simulate")
	seed  = flag.Uint64("sim.seed", 0, "run only this seed (0 = all)")
)

func run(t *testing.T, s uint64, faults bool) *World {
	t.Helper()
	w, err := New(Options{Seed: s, Workers: 3, Sessions: 3, Ticks: 400, Faults: faults})
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

func TestSimulationWithFaults(t *testing.T) {
	var total Stats
	for _, s := range seedList() {
		st := run(t, s, true).Stats
		total.Runs += st.Runs
		total.Steers += st.Steers
		total.Interrupts += st.Interrupts
		total.Crashes += st.Crashes
		total.ModelErrors += st.ModelErrors
		total.Takeovers += st.Takeovers
		total.OutcomeUnknown += st.OutcomeUnknown
		total.Retried += st.Retried
		total.Failed += st.Failed
		total.TurnLimited += st.TurnLimited
		total.NodeCrashes += st.NodeCrashes
		total.Redeliveries += st.Redeliveries
		total.Approvals += st.Approvals
		total.Denials += st.Denials
		total.Suspensions += st.Suspensions
		total.DeviceResults += st.DeviceResults
		total.Timeouts += st.Timeouts
	}
	t.Logf("coverage: %+v", total)
	if *seed != 0 {
		return
	}
	// 模拟必须真正触达关键路径，否则通过没有意义。
	for name, n := range map[string]int{
		"runs": total.Runs, "steers": total.Steers, "interrupts": total.Interrupts,
		"crashes": total.Crashes, "takeovers": total.Takeovers,
		"outcome unknown": total.OutcomeUnknown, "idempotent retries": total.Retried,
		"node crashes": total.NodeCrashes, "redeliveries": total.Redeliveries, "approvals": total.Approvals,
		"denials": total.Denials, "suspensions": total.Suspensions, "device results": total.DeviceResults,
		"timeouts": total.Timeouts,
	} {
		if n == 0 {
			t.Errorf("simulation never exercised %s", name)
		}
	}
}

func TestSimulationWithoutFaultsNeverFails(t *testing.T) {
	for _, s := range seedList() {
		if st := run(t, s, false).Stats; st.Failed != 0 || st.Takeovers != 0 || st.OutcomeUnknown != 0 {
			t.Fatalf("seed %d: fault-free run had failures/takeovers: %+v", s, st)
		}
	}
}

func TestSimulationIsDeterministic(t *testing.T) {
	for _, s := range []uint64{1, 7, 42} {
		a, b := run(t, s, true), run(t, s, true)
		if a.Fingerprint() != b.Fingerprint() {
			t.Fatalf("seed %d produced different logs", s)
		}
	}
}
