package capability_test

import (
	"context"
	"testing"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/capability"
	"yanshi/internal/clock"
	"yanshi/internal/model"
	"yanshi/internal/session"
)

func TestClockNowDefaultZone(t *testing.T) {
	now := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, zone, args string
		state            bool
		want             *time.Location
	}{
		{"session", "Asia/Shanghai", `{}`, true, time.FixedZone("Shanghai", 8*3600)},
		{"explicit", "Asia/Shanghai", `{"tz":"UTC"}`, true, time.UTC},
		{"no zone", "", `{}`, true, time.Local},
		{"no state", "", `{}`, false, time.Local},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := capability.Invocation{Arguments: tc.args}
			if tc.state {
				inv.State = &session.State{Created: &v1.SessionCreated{TimeZone: tc.zone}}
			}
			out, err := capability.ClockNow(clock.NewFake(now)).Invoke(context.Background(), inv)
			if err != nil || model.Text(out) != now.In(tc.want).Format(time.RFC3339+" Monday") {
				t.Fatalf("clock = %q, %v", model.Text(out), err)
			}
		})
	}
}

func TestClockNowReadOnly(t *testing.T) {
	if !capability.ClockNow(clock.NewFake(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC))).Spec().ReadOnly {
		t.Fatal("clock_now must provide read-only evidence")
	}
	if (capability.Spec{Idempotent: true, Risk: v1.Risk_RISK_LOW}).ReadOnly {
		t.Fatal("low risk and idempotence do not prove read-only")
	}
}
