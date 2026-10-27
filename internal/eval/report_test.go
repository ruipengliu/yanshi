package eval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReportTrialCosts(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(map[bool]string{false: "全部超时", true: "部分完成"}[completed], func(t *testing.T) {
			c := CaseResult{Name: "cost", Trials: 2, Failures: map[string]int{}}
			c.add(TrialResult{Err: "timeout", Tokens: 20, Duration: 2 * time.Second, Calls: 2, InputTokens: 10, CachedInputTokens: 1})
			c.add(TrialResult{Completed: completed, Tokens: 100, Duration: 10 * time.Second, Calls: 8, Compactions: 2, Takeovers: 1, InputTokens: 100, CachedInputTokens: 80})
			c.finish()
			if c.AvgTokens != 60 || c.AvgSeconds != 6 || c.AvgCalls != 5 {
				t.Fatalf("旧口径变化: %+v", c)
			}
			if c.Completed == nil || c.JudgeSamples == nil || *c.JudgeSamples != 0 || c.JudgeAvg != nil {
				t.Fatalf("样本: %+v", c)
			}
			if completed {
				if *c.Completed != 1 || c.Passed != 0 || c.CompletedCost.Tokens != 100 || c.CompletedCost.Seconds != 10 || c.CompletedCost.Calls != 8 || c.CompletedCost.CacheHit != .8 || c.IncompleteCost.Tokens != 20 || c.IncompleteCost.CacheHit != .1 {
					t.Fatalf("成本: %+v / %+v", c.CompletedCost, c.IncompleteCost)
				}
			} else if *c.Completed != 0 || c.CompletedCost != nil || c.IncompleteCost.Tokens != 120 || c.IncompleteCost.Seconds != 12 || c.IncompleteCost.Calls != 10 {
				t.Fatalf("全部超时: %+v", c)
			}
			data, err := json.Marshal(c)
			if err != nil || strings.Contains(string(data), "judge_avg") {
				t.Fatalf("无评分 JSON: %s / %v", data, err)
			}
			md := (&Report{Cases: []CaseResult{c}}).Markdown(nil, nil)
			if !strings.Contains(md, "—（0）") || !strings.Contains(md, "未完成合计") {
				t.Fatal(md)
			}
		})
	}
}

func TestReportScoreAndMetricSamples(t *testing.T) {
	c := CaseResult{Name: "samples", Trials: 2, Failures: map[string]int{}}
	c.add(TrialResult{Completed: true, JudgeScore: []int{0}, Metrics: map[string]float64{"turn 2: recall": .8}})
	c.add(TrialResult{Err: "timeout"})
	c.finish()
	if c.JudgeAvg == nil || *c.JudgeAvg != 0 || *c.JudgeSamples != 1 || score(c.JudgeAvg) != "0.00" {
		t.Fatalf("评分零分: %+v", c)
	}
	m := c.Metrics["turn 2: recall"]
	if m.N != 1 || m.Expected == nil || *m.Expected != 2 || !strings.Contains(m.String(), "(1/2)") {
		t.Fatalf("指标样本: %+v", m)
	}
}

func TestCompareMissingSamples(t *testing.T) {
	base := &Report{Cases: []CaseResult{{Name: "a", Completed: new(3), JudgeSamples: new(3), JudgeAvg: new(4.0), Metrics: map[string]Metric{"turn 2: recall": {Mean: .8, N: 3}}}}}
	for _, tt := range []struct {
		name string
		c    CaseResult
		want string
	}{
		{"缺指标", CaseResult{Metrics: nil}, "turn 2: recall 缺失"},
		{"少样本", CaseResult{Metrics: map[string]Metric{"turn 2: recall": {Mean: 1, N: 2}}}, "turn 2: recall 样本不足"},
		{"零样本", CaseResult{Metrics: map[string]Metric{"turn 2: recall": {N: 0}}}, "turn 2: recall 缺失"},
		{"少完成", CaseResult{Completed: new(0)}, "completed 样本不足"},
		{"少评分", CaseResult{JudgeSamples: new(0)}, "judge 样本不足"},
		{"缺评分", CaseResult{}, "judge 缺失"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.c.Name = "a"
			got := Compare(base, &Report{Cases: []CaseResult{tt.c}})
			if !strings.Contains(strings.Join(got, "\n"), tt.want) {
				t.Fatalf("回归=%v", got)
			}
		})
	}
}

func TestLoadLegacyReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	raw := `{"cases":[{"name":"old","trials":3,"passed":3,"pass_rate":1,"judge_avg":4,"avg_tokens":120,"avg_seconds":8,"avg_calls":2,"cache_hit":0.5,"metrics":{"turn 1: recall":{"mean":1,"std":0,"n":3}}}]}`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := LoadReport(path)
	if err != nil {
		t.Fatal(err)
	}
	c := r.Cases[0]
	if c.Completed != nil || c.JudgeSamples != nil || c.CompletedCost != nil || c.AvgTokens != 120 || c.JudgeAvg == nil || *c.JudgeAvg != 4 {
		t.Fatalf("旧格式: %+v", c)
	}
	cur := c
	cur.Completed, cur.JudgeSamples = new(0), new(0)
	if got := Compare(r, &Report{Cases: []CaseResult{cur}}); len(got) != 0 {
		t.Fatalf("未知字段导致回归: %v", got)
	}
	if got := Compare(r, r); len(got) != 0 {
		t.Fatal(got)
	}
	if !strings.Contains(r.Markdown(nil, nil), "—/3") {
		t.Fatal("旧完成数应为未知")
	}
	if err := r.WriteJSON(path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadReport(path); err != nil {
		t.Fatal(err)
	}
}

func TestReportCompletedAverages(t *testing.T) {
	c := CaseResult{Trials: 3, Failures: map[string]int{}}
	c.add(TrialResult{Completed: true, Tokens: 100, Duration: 10 * time.Second, Calls: 8, Compactions: 2, Takeovers: 1, InputTokens: 100, CachedInputTokens: 80})
	c.add(TrialResult{Completed: true, Tokens: 200, Duration: 20 * time.Second, Calls: 4, Compactions: 4, Takeovers: 3, InputTokens: 200, CachedInputTokens: 40})
	c.add(TrialResult{Err: "timeout", Tokens: 30, Duration: time.Second, Calls: 1})
	c.finish()
	got := c.CompletedCost
	if *c.Completed != 2 || got.Tokens != 150 || got.Seconds != 15 || got.Calls != 6 || got.Compactions != 3 || got.Takeovers != 2 || got.CacheHit != .4 || c.IncompleteCost.Tokens != 30 {
		t.Fatalf("完成均值: %+v，未完成合计: %+v", got, c.IncompleteCost)
	}
}
