package sim

import (
	"context"
	"errors"
	rand "math/rand/v2"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/eventlog"
	"yanshi/internal/model"
	"yanshi/internal/service"
	"yanshi/internal/workqueue"
)

func TestRedoWithFaults(t *testing.T) {
	total := parallelSeeds(t, seedList(), func(t *testing.T, s uint64) Stats {
		return run(t, s, Options{Faults: true, Redo: true, Schedules: true}).Stats
	})
	requireCoverage(t, total, "Redos", "RedoConflicts", "RedoDuplicates", "RedoCardRejects")
}

func TestRedoRandomStreamIsIndependent(t *testing.T) {
	for _, seed := range []uint64{1, 7, 42} {
		base, err := New(Options{Seed: seed, Workers: 1, Sessions: 1, Faults: true})
		if err != nil {
			t.Fatal(err)
		}
		withRedo, err := New(Options{Seed: seed, Workers: 1, Sessions: 1, Faults: true, Redo: true})
		if err != nil {
			t.Fatal(err)
		}
		for i := range 100 {
			for range i + 1 {
				withRedo.redoRNG.Uint64()
			}
			if base.rng.Uint64() != withRedo.rng.Uint64() {
				t.Fatalf("seed %d: redo draws changed legacy stream", seed)
			}
		}
	}
}

// redoTestSource 只固定边界抽样，之后仍走真实 PCG，避免 IntN 的拒绝抽样在常量源上空转。
type redoTestSource struct {
	prefix []uint64
	rest   rand.Source
}

func (s *redoTestSource) Uint64() uint64 {
	if len(s.prefix) > 0 {
		n := s.prefix[0]
		s.prefix = s.prefix[1:]
		return n
	}
	return s.rest.Uint64()
}

func TestRedoDoesNotReplaceClientAction(t *testing.T) {
	// 选中真实 act 的普通提交分支；Redo 不得抢走它，也不得跳过其主流抽样。
	var clientSeed uint64
	for {
		r := rand.New(rand.NewPCG(clientSeed, 7))
		x := r.Float64()
		if x >= 0.76 && x < 0.82 && r.Float64() >= 0.25 && r.Float64() >= 0.25 && r.Float64() >= 0.1 {
			break
		}
		clientSeed++
	}
	var firstInput string
	var nextDraw uint64
	for _, redo := range []bool{false, true} {
		w, err := New(Options{Seed: 24, Workers: 1, Sessions: 1, Redo: redo})
		if err != nil {
			t.Fatal(err)
		}
		w.rng = rand.New(rand.NewPCG(clientSeed, 7))
		w.redoRNG = rand.New(&redoTestSource{prefix: []uint64{0, 0, 0}, rest: rand.NewPCG(1, 2)})
		if err := w.act(); err != nil {
			t.Fatal(err)
		}
		events, err := eventlog.ReadAll(t.Context(), w.log, w.sessions[0], 0)
		if err != nil {
			t.Fatal(err)
		}
		input := ""
		for _, e := range events {
			if r := e.GetRunRequested(); r != nil {
				input = model.Text(r.GetInput())
				break
			}
		}
		if input == "" {
			t.Fatal("normal client action did not submit input")
		}
		draw := w.rng.Uint64()
		if !redo {
			firstInput, nextDraw = input, draw
		} else if input != firstInput || draw != nextDraw {
			t.Fatal("redo replaced the normal input or changed its random draws")
		}
	}
}

func TestRedoActionKeepsLegacyRandomStream(t *testing.T) {
	w, err := New(Options{Seed: 24, Workers: 1, Sessions: 1, Redo: true})
	if err != nil {
		t.Fatal(err)
	}
	legacy := rand.New(rand.NewPCG(3, 4))
	w.rng = legacy
	want := rand.New(rand.NewPCG(3, 4))
	// 强制附加动作生成 probe，并在提交返回前推进 Worker，覆盖间接消耗随机数的路径。
	w.redoRNG = rand.New(&redoTestSource{prefix: []uint64{0, 0, 0, 0, 0}, rest: rand.NewPCG(5, 6)})
	if err := w.maybeRedo(); err != nil {
		t.Fatal(err)
	}
	if w.nextInput == 0 || w.Stats.EnqueueInterleavings == 0 {
		t.Fatal("redo did not exercise submission and the worker interleaving")
	}
	if w.rng != legacy {
		t.Fatal("redo did not restore the legacy stream")
	}
	for range 100 {
		if w.rng.Uint64() != want.Uint64() {
			t.Fatal("redo submission or worker consumed the legacy stream")
		}
	}
}

func TestRedoSeed24DoesWork(t *testing.T) {
	// 与 PG 差分失败的内存侧使用相同选项，不依赖 PostgreSQL 就能锁定空转回归。
	w := run(t, 24, Options{Redo: true, Tiers: true, MaintainDues: true, Schedules: true,
		LongRuns: true, Workers: 3, Sessions: 2, Ticks: 800, Faults: true})
	if w.Stats.Runs == 0 && w.Stats.CallTasks == 0 {
		t.Fatal("seed 24: simulation did nothing")
	}
}

type redoLostWaking struct {
	eventlog.WakingLog
	lost bool
}

func (w *redoLostWaking) AppendAndEnqueue(ctx context.Context, sid string, seq uint64, class workqueue.Class, events ...*v1.Event) (uint64, error) {
	n, err := w.WakingLog.AppendAndEnqueue(ctx, sid, seq, class, events...)
	if err == nil && !w.lost {
		w.lost = true
		return n, &storeFault{written: true}
	}
	return n, err
}

func TestRunCoverageSurvivesLostResponseAndDeletion(t *testing.T) {
	w, err := New(Options{Seed: 24, Workers: 1, Sessions: 1})
	if err != nil {
		t.Fatal(err)
	}
	w.store.Waking = &redoLostWaking{WakingLog: w.stores.Waking}
	sid := w.sessions[0]
	input := model.TextBlocks("lost response")
	_, err = w.svc.SubmitWithID(t.Context(), sid, "lost", input)
	var sf *storeFault
	if !errors.As(err, &sf) || !sf.written {
		t.Fatalf("expected a committed write with a lost response: %v", err)
	}
	if err := w.retryUncertain(sid, "lost", input, true); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := w.CheckInvariants(); err != nil {
			t.Fatal(err)
		}
		if w.Stats.Runs != 1 {
			t.Fatalf("committed run count = %d, want 1", w.Stats.Runs)
		}
	}
	if err := w.deleteSession(); err != nil {
		t.Fatal(err)
	}
	if err := w.quiesce(); err != nil {
		t.Fatal(err)
	}
	if w.Stats.Runs != 1 {
		t.Fatal("deletion erased committed run coverage")
	}
}

func TestRunCoverageIncludesSchedules(t *testing.T) {
	w, err := New(Options{Seed: 24, Workers: 1, Sessions: 1, Schedules: true})
	if err != nil {
		t.Fatal(err)
	}
	sid := w.sessions[0]
	_, err = w.svc.CreateSchedule(t.Context(), sid, service.CreateScheduleRequest{ScheduleID: "due", Input: model.TextBlocks("reminder"),
		At: w.clock.Now().Add(time.Second).Format("2006-01-02T15:04:05"), TimeZone: "UTC", By: "service"})
	if err != nil {
		t.Fatal(err)
	}
	w.clock.Advance(time.Second)
	for range 20 {
		if err := w.stepWorker(0); err != nil {
			t.Fatal(err)
		}
		events, err := eventlog.ReadAll(t.Context(), w.log, sid, 0)
		if err != nil {
			t.Fatal(err)
		}
		if countPayload[*v1.Event_RunRequested](events) > 0 {
			break
		}
	}
	if err := w.CheckInvariants(); err != nil {
		t.Fatal(err)
	}
	if w.Stats.Runs != 1 {
		t.Fatalf("scheduled run count = %d, want 1", w.Stats.Runs)
	}
}
