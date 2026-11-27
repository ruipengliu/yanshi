package eval

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"yanshi/internal/eval/xinlifixture"
)

func TestCaptureScenarioDates(t *testing.T) {
	cases, err := LoadSuite("../../apps/xinli/evals")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*Case{}
	for _, c := range cases {
		byName[c.Name] = c
	}
	for _, name := range []string{"capture-multiday", "capture-multiday-noisy"} {
		c := byName[name]
		for i, day := range []string{"2026-10-12", "2026-10-13", "2026-10-13", "2026-10-14", "2026-10-15"} {
			if c.Turns[i].BusinessDay != day {
				t.Fatalf("%s turn %d: business_day = %s", name, i+1, c.Turns[i].BusinessDay)
			}
		}
		if c.Turns[5].Agent != "xinli-assistant" || !c.Turns[5].NewSession || c.Turns[5].BusinessDay != "" || c.Turns[6].BusinessDay != "" {
			t.Fatalf("%s assistant questions changed the final scenario day", name)
		}
	}
	for _, name := range []string{
		"capture-workday", "capture-workday-oneshot", "capture-multiday", "capture-multiday-noisy",
		"capture-metadata-unverified", "capture-metadata-close-rejected", "capture-metadata-reschedule-rejected", "capture-metadata-qualified-ref", "capture-metadata-anonymous-speakers",
	} {
		c := byName[name]
		at, err := time.Parse(time.RFC3339, c.Now)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		day := captureBusinessDay(at)
		for i, turn := range c.Turns {
			if turn.BusinessDay != "" {
				day = turn.BusinessDay
			}
			for _, g := range turn.Captures {
				spoken, err := captureTime(g.At)
				if err != nil || captureBusinessDay(spoken) != day || spoken.In(captureZone).Format("15:04") > "18:30" {
					t.Fatalf("%s turn %d: capture %s is outside scenario business day %s at 18:30", name, i+1, g.At, day)
				}
			}
		}
	}
	// 分片和合并版必须连原话、元数据及主题一起相同，防止两种整理节奏的比较混入数据差异。
	var split []CaptureSegment
	for _, turn := range byName["capture-workday"].Turns {
		split = append(split, turn.Captures...)
	}
	all := byName["capture-workday-oneshot"].Turns[0].Captures
	if !reflect.DeepEqual(split, all) {
		t.Fatal("workday merged data differs from its shards")
	}
	for _, g := range all {
		clock := g.At[len("2026-10-23 "):]
		want := ""
		switch {
		case clock >= "15:34" && clock <= "15:39":
			want = "kangning"
		case clock >= "15:40" && clock <= "15:43":
			want = "career"
		case clock >= "17:36" && clock <= "17:38":
			want = "renovation"
		}
		if want != "" && g.Topic != want {
			t.Fatalf("topic at %s = %s, want %s", g.At, g.Topic, want)
		}
	}
	for _, name := range []string{"capture-workday", "capture-workday-oneshot"} {
		last := byName[name].Turns[len(byName[name].Turns)-1]
		x := last.Expect.Xinli
		if !last.JudgeState || !slices.Contains(x.NoText, "失眠") || x.CommitmentsCount == nil || x.CommitmentsCount.Min != nil || x.CommitmentsCount.Max != nil || x.StaleOpen == nil || x.Grouping.Fragmentation != nil || x.Grouping.Purity != nil {
			t.Fatalf("%s final-state checks are inconsistent", name)
		}
		if x.Gold["commitment_precision"] != 0.90 || x.Gold["commitment_recall"] != 0.85 || x.Gold["reminder_precision"] != 0.90 {
			t.Fatalf("%s missing gold quality thresholds", name)
		}
	}
}

func TestCommitmentCountReportOnly(t *testing.T) {
	st := &xinlifixture.State{Commitments: []xinlifixture.Commitment{
		{ID: "seed", Content: "预置"},
		{ID: "open", Session: "s", Status: "open"},
		{ID: "done", Session: "s", Status: "done"},
		{ID: "cancelled", Session: "s", Status: "cancelled"},
		{ID: "merged", Session: "s", MergedInto: "open"},
	}}
	for _, r := range []*Range{nil, {}, {Max: new(35)}} {
		_, _, metrics := (&instance{}).check(t.Context(), 1, Turn{Expect: Expect{Xinli: &XinliExpect{CommitmentsCount: r}}}, &observation{status: "completed", xinli: st})
		count, reported := metrics["commitments_count"]
		wantReport := r != nil && r.Max == nil
		if reported != wantReport || reported && count != 3 {
			t.Fatalf("count metric = %v, want report=%v with count 3", metrics, wantReport)
		}
		if !reported {
			continue
		}
		result := CaseResult{Name: "diagnostic", Trials: 1}
		result.add(TrialResult{Passed: true, Completed: true, Metrics: map[string]float64{"turn 1: commitments_count": count}})
		result.finish()
		rep := &Report{Cases: []CaseResult{result}}
		if got := result.Metrics["turn 1: commitments_count"].Mean; got != 3 || !strings.Contains(rep.Markdown(nil, nil), "commitments_count") {
			t.Fatalf("count omitted from report: %+v", result.Metrics)
		}
	}
	for _, count := range []float64{1, 50} {
		if worse("turn 1: commitments_count", Metric{Mean: 3, N: 3}, Metric{Mean: count, N: 3}) {
			t.Fatal("diagnostic count became a regression gate")
		}
	}
}
