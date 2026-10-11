package moderation

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fixed struct {
	v     Verdict
	err   error
	block time.Duration
	calls *int
}

func (f fixed) Check(ctx context.Context, _ Request) (Verdict, error) {
	*f.calls++
	if f.block > 0 {
		select {
		case <-ctx.Done():
			return Verdict{}, ctx.Err()
		case <-time.After(f.block):
		}
	}
	return f.v, f.err
}

func TestFailover(t *testing.T) {
	down := errors.New("unavailable")
	for name, tc := range map[string]struct {
		primary, backup fixed
		block           bool
		err             bool
		backupCalls     int
	}{
		"primary allows":                {primary: fixed{}, backup: fixed{v: Verdict{Block: true}}, backupCalls: 0},
		"primary blocks, backup unused": {primary: fixed{v: Verdict{Block: true}}, backup: fixed{}, block: true, backupCalls: 0},
		"primary down, backup blocks":   {primary: fixed{err: down}, backup: fixed{v: Verdict{Block: true}}, block: true, backupCalls: 1},
		"primary times out":             {primary: fixed{block: time.Second}, backup: fixed{}, backupCalls: 1},
		"all down":                      {primary: fixed{err: down}, backup: fixed{err: down}, err: true, backupCalls: 1},
	} {
		t.Run(name, func(t *testing.T) {
			var pc, bc int
			tc.primary.calls, tc.backup.calls = &pc, &bc
			f := Failover{Names: []string{"a", "b"}, Providers: []Moderator{tc.primary, tc.backup}, Timeout: 20 * time.Millisecond}
			v, err := f.Check(context.Background(), Request{Stage: Output, Text: "x"})
			if (err != nil) != tc.err || v.Block != tc.block || bc != tc.backupCalls {
				t.Fatalf("verdict %+v, err %v, backup calls %d", v, err, bc)
			}
		})
	}
}
