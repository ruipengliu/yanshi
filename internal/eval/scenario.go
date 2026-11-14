package eval

import (
	"context"
	"fmt"
	"time"
)

func isolatedClock(c *Case) bool {
	return c.Now != "" || advancesSchedule(c)
}

func validateScenarioTime(c *Case) error {
	var day string
	if c.Now != "" {
		at, err := time.Parse(time.RFC3339Nano, c.Now)
		if err != nil {
			return fmt.Errorf("now must be RFC3339 with time zone: %w", err)
		}
		day = captureBusinessDay(at)
	}
	for i, turn := range c.Turns {
		if turn.BusinessDay == "" {
			continue
		}
		if c.Now == "" {
			return fmt.Errorf("turn %d: business_day requires now", i+1)
		}
		if _, err := time.Parse(time.DateOnly, turn.BusinessDay); err != nil {
			return fmt.Errorf("turn %d: business_day must be YYYY-MM-DD", i+1)
		}
		if turn.BusinessDay < day {
			return fmt.Errorf("turn %d: business_day cannot go backwards", i+1)
		}
		if turn.BusinessDay != day && i > 0 && !turn.NewSession {
			return fmt.Errorf("turn %d: changing business_day requires new_session", i+1)
		}
		if turn.Redo || turn.AdvanceToNextSchedule || turn.AwaitBackground {
			return fmt.Errorf("turn %d: business_day cannot accompany redo, advance_to_next_schedule or await_background", i+1)
		}
		day = turn.BusinessDay
	}
	return nil
}

// 场景初始化允许历史偏移；之后仍只前拨，租约、Memory 与 Persona 共用相同的真实时间偏移。
func scenarioClock(cases []*Case) (*offsetClock, bool, error) {
	clk := &offsetClock{}
	for _, c := range cases {
		if c.Now == "" {
			continue
		}
		if len(cases) != 1 {
			return nil, false, fmt.Errorf("now requires an isolated trial")
		}
		if err := validateScenarioTime(c); err != nil {
			return nil, false, err
		}
		at, _ := time.Parse(time.RFC3339Nano, c.Now)
		clk.offset = at.Sub(time.Now())
		return clk, true, nil
	}
	return clk, false, nil
}

func (in *instance) syncScenarioClock(ctx context.Context) error {
	if !in.scenario || in.xinli == nil {
		return nil
	}
	return in.xinli.SetClockOffset(ctx, in.clock.Offset())
}

func (in *instance) advanceBusinessDay(ctx context.Context, initial, day string) error {
	at, err := time.Parse(time.RFC3339Nano, initial)
	if err != nil {
		return err
	}
	d, err := time.ParseInLocation(time.DateOnly, day, captureZone)
	if err != nil {
		return err
	}
	local := at.In(captureZone)
	// 保留起始场景的当地钟点；不能把第二天的数据放到当天零点、使尚未结束的时段读不到。
	target := time.Date(d.Year(), d.Month(), d.Day(), local.Hour(), local.Minute(), local.Second(), local.Nanosecond(), captureZone)
	in.clock.AdvanceTo(target)
	return in.syncScenarioClock(ctx)
}
