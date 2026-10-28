package sim

import "testing"

// 档位会改变额度与执行路径，覆盖独立开启，旧综合场景继续要求包括 CardAnswers 的全部既有覆盖。
func TestTiersWithFaults(t *testing.T) {
	total := parallelSeeds(t, seedList(), func(t *testing.T, s uint64) Stats {
		return run(t, s, Options{Faults: true, Attested: true, Tiers: true}).Stats
	})
	requireCoverage(t, total, "TiersSet", "TiersWithdrawn", "TierWriterCrashes", "TierCrashRecoveries", "AccountDeletions")
}

func TestTierRandomStreamIsIndependent(t *testing.T) {
	for _, s := range []uint64{1, 7, 42} {
		base, err := New(Options{Seed: s, Workers: 1, Sessions: 1, Faults: true})
		if err != nil {
			t.Fatal(err)
		}
		tiered, err := New(Options{Seed: s, Workers: 1, Sessions: 1, Faults: true, Tiers: true})
		if err != nil {
			t.Fatal(err)
		}
		for i := range 100 {
			// 档位初始化与独立流的任意多次推进均不能改变旧流的下一个值。
			for range i + 1 {
				tiered.tierRNG.Uint64()
			}
			if a, b := base.rng.Uint64(), tiered.rng.Uint64(); a != b {
				t.Fatalf("seed %d: legacy stream changed after tier draw %d: %d != %d", s, i, a, b)
			}
		}
	}
}
