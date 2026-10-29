package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"yanshi/internal/oneshot"
)

// 显式限定无 Session 的用例字段，防止把 Session 操作或断言静默忽略后误报通过。
func validateOneshotCase(c *Case) error {
	if c.Oneshot == "" {
		for _, t := range c.Turns {
			if t.Expect.OutputSchemaValid != nil {
				return fmt.Errorf("output_schema_valid requires oneshot")
			}
		}
		return nil
	}
	if c.Agent != "" {
		return fmt.Errorf("agent and oneshot are mutually exclusive")
	}
	rest := *c
	rest.Name, rest.Description, rest.Oneshot = "", "", ""
	rest.Trials, rest.Timeout, rest.Turns = 0, 0, nil
	if !reflect.ValueOf(rest).IsZero() {
		return fmt.Errorf("oneshot cases cannot configure Session setup or operations")
	}
	for i, t := range c.Turns {
		if strings.TrimSpace(t.Input) == "" {
			return fmt.Errorf("oneshot turn %d requires input", i+1)
		}
		rest := t
		rest.Input, rest.Expect = "", Expect{}
		if !reflect.ValueOf(rest).IsZero() {
			return fmt.Errorf("oneshot turn %d only accepts input and expect", i+1)
		}
		e := t.Expect
		e.Status, e.Judge = "", ""
		e.ReplyContains, e.ReplyExcludes, e.ReplyMaxChars, e.OutputSchemaValid = nil, nil, 0, nil
		if !reflect.ValueOf(e).IsZero() {
			return fmt.Errorf("oneshot turn %d only supports reply, status, schema and judge assertions", i+1)
		}
	}
	return nil
}

func deriveOneshots(cfg Config, cases []*Case) (*oneshot.Registry, error) {
	var defs []*oneshot.Def
	seen := map[string]bool{}
	for _, c := range cases {
		if c.Oneshot == "" {
			continue
		}
		if err := validateOneshotCase(c); err != nil {
			return nil, err
		}
		if seen[c.Oneshot] {
			continue
		}
		d, err := cfg.Oneshots.Default(c.Oneshot)
		if cfg.OneshotVersion != "" {
			d, err = cfg.Oneshots.Get(c.Oneshot, cfg.OneshotVersion)
		}
		if err != nil {
			return nil, fmt.Errorf("case %s: %w", c.Name, err)
		}
		cp := *d
		if cfg.ModelOverride != "" {
			cp.Model = cfg.ModelOverride
		}
		defs = append(defs, &cp)
		seen[c.Oneshot] = true
	}
	if len(defs) == 0 {
		return nil, nil
	}
	return oneshot.NewRegistry(defs...)
}

func (in *instance) oneshotTrial(ctx context.Context, c *Case, bl, endUser string) (tr TrialResult) {
	for i, t := range c.Turns {
		timeout := c.Timeout
		if timeout <= 0 {
			timeout = in.cfg.TurnTimeout
		}
		turnCtx, cancel := context.WithTimeout(ctx, timeout)
		result, err := in.oneshots.Call(turnCtx, oneshot.Request{BusinessLine: bl, EndUser: endUser, Name: c.Oneshot, Input: t.Input})
		cancel()
		if result != nil {
			tr.Tokens += result.Usage.GetInputTokens() + result.Usage.GetOutputTokens()
		}
		if err != nil {
			tr.Err = fmt.Sprintf("turn %d: %v", i+1, err)
			return tr
		}
		reply, ok := result.Output.(string)
		if !ok {
			b, err := json.Marshal(result.Output)
			if err != nil {
				tr.Err = err.Error()
				return tr
			}
			reply = string(b)
		}
		// 对完整 JSON 文本做断言与评分，不给业务字段引入内核特判。
		o := &observation{input: t.Input, reply: reply, status: "completed"}
		assertions, score, _ := in.check(ctx, i+1, t, o)
		tr.Assertions = append(tr.Assertions, assertions...)
		if score > 0 {
			tr.JudgeScore = append(tr.JudgeScore, score)
		}
		d, _ := in.oneshots.Registry.Get(result.Name, result.Version)
		checkSchema := d.HasSchema()
		if t.Expect.OutputSchemaValid != nil {
			checkSchema = *t.Expect.OutputSchemaValid
		}
		if checkSchema {
			_, category := d.ValidateOutput(reply)
			tr.Assertions = append(tr.Assertions, Assertion{Turn: i + 1, Name: "output_schema_valid", Pass: d.HasSchema() && category == "", Detail: category})
		}
	}
	tr.Completed = true
	return tr
}
