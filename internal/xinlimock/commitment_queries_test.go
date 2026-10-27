package xinlimock

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestListCommitments(t *testing.T) {
	x := newServer(nil)
	at := x.now()
	x.Seed("bl", "u", SeedData{Projects: []Project{{ID: "P-1", Name: "办公室"}}, Commitments: []SeedCommitment{
		{ID: "C-1", Kind: "mine", Content: "成本核算表", Counterpart: "张总", DueDate: "2026-10-26", Project: "P-1", SpokenAt: at},
		{ID: "C-2", Kind: "mine", Content: "打印合同", Counterpart: "张总", DueDate: "2026-10-26", SpokenAt: at.Add(-time.Hour), Unverified: "缺少原话"},
		{ID: "C-3", Kind: "mine", Content: "取快递"},
		{ID: "C-4", Kind: "theirs", Content: "提供报价", DueDate: "2026-10-25"},
		{ID: "C-5", Kind: "mine", Content: "完成的事", Status: "done"},
		{ID: "C-6", Kind: "mine", Content: "取消的事", Status: "cancelled"},
		{ID: "C-7", Kind: "mine", Content: "重复合同", Revisions: []SeedRevision{{Op: "merge", Fields: map[string]string{"merged_into": "C-2"}}}},
	}})
	cases := []struct {
		name   string
		args   M
		want   []string
		absent []string
	}{
		{"默认", M{}, []string{"共 4 条", "未核对：缺少原话", "2026-10-26（星期一）", "修订 1 次"}, []string{"C-5 |", "C-6 |", "C-7 |"}},
		{"本人", M{"kind": "mine"}, []string{"共 3 条", "C-3 |"}, []string{"C-4 |"}},
		{"别人", M{"kind": "theirs"}, []string{"C-4 |", "共 1 条"}, []string{"C-1 |"}},
		{"完成", M{"status": "done"}, []string{"C-5 |", "共 1 条"}, nil},
		{"取消", M{"status": "cancelled"}, []string{"C-6 |", "共 1 条"}, nil},
		{"全部", M{"status": "all"}, []string{"共 6 条", "C-5 |", "C-6 |"}, []string{"C-7 |"}},
		{"期限", M{"due_from": "2026-10-26", "due_to": "2026-10-26"}, []string{"共 2 条"}, []string{"C-3 |", "C-4 |"}},
		{"联合", M{"kind": "mine", "counterpart": "张", "project": "P-1", "query": "成本核算表"}, []string{"C-1 |", "项目：P-1 办公室", "共 1 条"}, nil},
		{"主题对方", M{"query": "张总"}, []string{"C-1 |", "C-2 |"}, nil},
		{"空", M{"query": "完全无关主题"}, []string{"没有符合条件的承诺", "共 0 条"}, nil},
		{"日期非法", M{"due_to": "2026-02-30"}, []string{"期限须为"}, nil},
		{"范围非法", M{"due_from": "2026-10-27", "due_to": "2026-10-26"}, []string{"不能晚于"}, nil},
		{"状态非法", M{"status": "bad"}, []string{"status 须为"}, nil},
		{"责任方非法", M{"kind": "bad"}, []string{"kind 须为"}, nil},
		{"游标非法", M{"after": "missing"}, []string{"after 无效"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := invoke(x, "", "list_commitments", tc.args)
			contains(t, "列表", got, tc.want...)
			for _, s := range tc.absent {
				if strings.Contains(got, s) {
					t.Fatalf("意外包含 %s: %s", s, got)
				}
			}
		})
	}
	got := invoke(x, "", "list_commitments", M{})
	order := []string{"C-4 |", "C-2 |", "C-1 |", "C-3 |"}
	for i := 1; i < len(order); i++ {
		if strings.Index(got, order[i-1]) >= strings.Index(got, order[i]) {
			t.Fatal(got)
		}
	}
}

func TestListCommitmentsPages(t *testing.T) {
	x := newServer(nil)
	var ms []SeedCommitment
	for i := 0; i < 65; i++ {
		ms = append(ms, SeedCommitment{ID: fmt.Sprintf("C-%04d", i+1), Kind: "mine", Content: "待办"})
	}
	x.Seed("bl", "u", SeedData{Commitments: ms})
	for _, p := range []struct {
		after, next string
		n           int
	}{{"", "C-0030", 30}, {"C-0030", "C-0060", 30}, {"C-0060", "无", 5}} {
		got := invoke(x, "", "list_commitments", M{"after": p.after})
		contains(t, "分页", got, "共 65 条", "下一页 after："+p.next)
		if strings.Count(got, " | mine |") != p.n {
			t.Fatal(got)
		}
	}
}

func TestCommitmentHistory(t *testing.T) {
	x := newServer(nil)
	now := x.now()
	old := now.Add(-8 * 24 * time.Hour)
	x.Seed("bl", "u", SeedData{Commitments: []SeedCommitment{
		{ID: "C-1", Kind: "mine", Content: "成本核算表", DueDate: "2026-10-24", SpokenAt: old, Refs: []string{"#1.0"}, Revisions: []SeedRevision{
			{Author: AuthorDay, Session: "day1", Op: "revise", Fields: map[string]string{"due_date": "2026-10-26"}, SpokenAt: old.Add(time.Hour), RecordedAt: now, Refs: []string{"#2.0"}, Reason: "原话改期"},
			{Author: AuthorProject, Op: "revise", Fields: map[string]string{"due_date": "2026-10-28", "content": "新版成本核算表"}, SpokenAt: now.Add(-time.Hour), RecordedAt: now, Exchanges: []string{"x3.0"}},
			{Author: AuthorUser, Op: "close", Fields: map[string]string{"status": "done"}, RecordedAt: now},
		}},
		{ID: "C-2", Kind: "mine", Content: "重复表", Revisions: []SeedRevision{{Op: "merge", Fields: map[string]string{"merged_into": "C-1"}}}},
		{ID: "C-3", Kind: "mine", Content: "更早重复表", Revisions: []SeedRevision{{Op: "merge", Fields: map[string]string{"merged_into": "C-2"}}}},
	}})
	got := invoke(x, "", "commitment_history", M{"id": "C-1"})
	contains(t, "历史", got, "当前值：C-1 | mine | done | 新版成本核算表", "期限：无 → 2026-10-24（星期六）", "2026-10-24（星期六） → 2026-10-26（星期一）", "2026-10-26（星期一） → 2026-10-28（星期三）", "内容：成本核算表 → 新版成本核算表", "状态：open → done", "依据：原话", "依据：摘要", "依据：用户本人", "原话已过保留期", "day1 #2.0", "x3.0", "说话时间：未记录", "被合并者 C-2", "被合并者 C-3", "合并到：无 → C-1")
	contains(t, "被合并", invoke(x, "", "commitment_history", M{"id": "C-2"}), "已合并到 C-1")
	contains(t, "不存在", invoke(x, "", "commitment_history", M{"id": "bad"}), "没有这个承诺")
	// 只读查询不改账本；新建的旧值也不应被历史展示覆盖。
	again := invoke(x, "", "commitment_history", M{"id": "C-1"})
	if again != got {
		t.Fatal(again)
	}
}

func TestHistorySpokenOrderAndRetention(t *testing.T) {
	x := newServer(nil)
	now := x.now()
	x.Seed("bl", "u", SeedData{Commitments: []SeedCommitment{{ID: "C-1", Kind: "mine", Content: "表", DueDate: "2026-10-24", SpokenAt: now.Add(-7 * 24 * time.Hour), Refs: []string{"#1.0"}, Revisions: []SeedRevision{
		{Op: "revise", Fields: map[string]string{"due_date": "2026-10-28"}, SpokenAt: now.Add(-time.Hour)},
		{Op: "revise", Fields: map[string]string{"due_date": "2026-10-26"}, SpokenAt: now.Add(-2 * time.Hour)},
	}}}})
	got := invoke(x, "", "commitment_history", M{"id": "C-1"})
	contains(t, "重放", got, "2026-10-24（星期六） → 2026-10-26（星期一）", "2026-10-26（星期一） → 2026-10-28（星期三）")
	if strings.Contains(got, "原话已过保留期") {
		t.Fatal(got)
	}
}

func TestCommitmentQueriesReadOnlyAndSeedYAML(t *testing.T) {
	for _, name := range []string{"list_commitments", "commitment_history"} {
		found := false
		for _, tool := range tools {
			if tool.tool.Name == name {
				found = true
				if tool.tool.Annotations == nil || !tool.tool.Annotations.ReadOnlyHint {
					t.Fatal(name)
				}
			}
		}
		if !found {
			t.Fatal(name)
		}
	}
	var data SeedData
	err := yaml.Unmarshal([]byte(`commitments:
  - id: C-1
    kind: mine
    content: 成本核算表
    spoken_at: 2026-10-20T09:00:00+08:00
    revisions:
      - op: close
        author: user
        recorded_at: 2026-10-21T09:00:00+08:00
        fields: {status: cancelled}
`), &data)
	if err != nil {
		t.Fatal(err)
	}
	x := newServer(nil)
	x.Seed("bl", "u", data)
	contains(t, "预置", invoke(x, "", "commitment_history", M{"id": "C-1"}), "状态：open → cancelled", "取消", "依据：用户本人")
}
