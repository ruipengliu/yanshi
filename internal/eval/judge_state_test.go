package eval

import (
	"strings"
	"testing"

	"yanshi/internal/eval/xinlifixture"
)

func TestJudgeStateOptIn(t *testing.T) {
	o := &observation{input: "整理今天", steps: []string{"调用工具", "收到回执"}, reply: "已整理", xinli: &xinlifixture.State{
		Exchanges: []xinlifixture.Exchange{
			{ID: "current", Summary: "最终纪要"},
			{ID: "old", Summary: "被合并的旧纪要", MergedInto: "current"},
		},
		Days: []xinlifixture.Day{{Date: "2026-10-23", Headline: "最终每日结果"}},
		Commitments: []xinlifixture.Commitment{
			{ID: "C-1", Kind: "mine", Content: "报价单", DueDate: "2026-10-30", Status: "open", Unverified: "识别不可靠", Revisions: []xinlifixture.Revision{{Op: "create"}, {Op: "revise"}}},
			{ID: "C-2", Content: "被合并的承诺", MergedInto: "C-1"},
			{ID: "C-3", Content: "已经完成的承诺", Status: "done"},
		},
	}}
	want := "评分标准：标准\n\n用户输入：整理今天\n\n助手的过程：\n- 调用工具\n- 收到回执\n\n助手本轮的回复（全部文本）：\n已整理\n"
	old, err := judgeInput("标准", o, false)
	if err != nil || old != want {
		t.Fatalf("legacy judge input changed: %q, %v", old, err)
	}
	withState, err := judgeInput("标准", o, true)
	if err != nil || !strings.HasPrefix(withState, want) {
		t.Fatalf("judge state input = %q, %v", withState, err)
	}
	for _, text := range []string{"最终纪要", "最终每日结果", "报价单", `"due_date":"2026-10-30"`, `"status":"open"`, `"rev":2`, "识别不可靠", "已经完成的承诺"} {
		if !strings.Contains(withState, text) {
			t.Fatalf("judge input missing %q: %s", text, withState)
		}
	}
	for _, text := range []string{"被合并的旧纪要", "被合并的承诺", `"changesets"`, `"revisions"`} {
		if strings.Contains(withState, text) {
			t.Fatalf("judge input contains obsolete/history state %q", text)
		}
	}
	o.xinli = nil
	if got, err := judgeInput("标准", o, false); err != nil || got != want {
		t.Fatal("legacy judge now requires state")
	}
	if _, err := judgeInput("标准", o, true); err == nil {
		t.Fatal("judge_state silently omitted a missing snapshot")
	}
	dir := t.TempDir()
	writeCase(t, dir, "state", "name: state\nsetup: {xinli: {}}\nturns:\n  - input: hi\n    judge_state: true\n    expect: {judge: 按最终纪要评分}\n")
	cases, err := LoadSuite(dir)
	if err != nil || !cases[0].Turns[0].JudgeState {
		t.Fatalf("judge_state load = %+v, %v", cases, err)
	}
}
