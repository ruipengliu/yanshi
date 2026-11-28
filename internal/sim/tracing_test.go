package sim

import (
	"fmt"
	"testing"

	"yanshi/internal/tracing"
)

func TestTracingPreservesSimulationDeterminism(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(fmt.Sprintf("external=%t", external), func(t *testing.T) {
			for _, seed := range []uint64{1, 7, 42} {
				t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
					t.Parallel()
					opts := Options{Faults: true, Attested: true, LongRuns: seed == 7, ExternalLoop: external}
					without := run(t, seed, opts).Fingerprint()
					tracer, recorder := tracing.NewRecorder()
					opts.tracer = tracer
					with := run(t, seed, opts).Fingerprint()
					if without != with {
						t.Fatal("tracing changed the simulation fingerprint")
					}
					if len(recorder.Records()) == 0 {
						t.Fatal("tracing recorded no spans")
					}
				})
			}
		})
	}
}
