package capability

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/clock"
)

func text(s string) []*v1.ContentBlock {
	return []*v1.ContentBlock{{Kind: &v1.ContentBlock_Text{Text: &v1.Text{Text: s}}}}
}

// ClockNow 返回当前时间，可选时区。
func ClockNow(c clock.Clock) Capability {
	return Func{
		S: Spec{
			Name:        "clock_now",
			Description: "Returns the current date and time. Optional IANA time zone, e.g. Asia/Shanghai.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"tz":{"type":"string"}}}`),
			Idempotent:  true,
		},
		Fn: func(_ context.Context, inv Invocation) ([]*v1.ContentBlock, error) {
			var in struct {
				TZ string `json:"tz"`
			}
			if inv.Arguments != "" {
				if err := json.Unmarshal([]byte(inv.Arguments), &in); err != nil {
					return nil, fmt.Errorf("invalid arguments: %w", err)
				}
			}
			loc := time.Local
			if in.TZ != "" {
				l, err := time.LoadLocation(in.TZ)
				if err != nil {
					return nil, err
				}
				loc = l
			}
			return text(c.Now().In(loc).Format(time.RFC3339 + " Monday")), nil
		},
	}
}
