package xinlimock

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"yanshi/internal/mcpcap"
)

// fakeBusiness 记录归集时创建的 Session 与提交的输入。
type fakeBusiness struct {
	mu       sync.Mutex
	sessions []string
	inputs   map[string][]string // Session → 输入 ID（去重后）
	texts    map[string]string
	done     chan struct{}
}

func (f *fakeBusiness) CreateSession(_ context.Context, _, _, agent string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if agent != ProjectAgent && agent != WeekAgent {
		panic(agent)
	}
	sid := "ses_p" + string(rune('0'+len(f.sessions)))
	f.sessions = append(f.sessions, sid)
	return sid, nil
}

func (f *fakeBusiness) Submit(_ context.Context, sid, inputID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.inputs[sid] {
		if id == inputID {
			return nil
		}
	}
	f.inputs[sid] = append(f.inputs[sid], inputID)
	f.texts[sid] = text
	f.done <- struct{}{}
	return nil
}

func invoke(x *Server, session, name string, args any, cites ...mcpcap.Citation) string {
	raw, _ := json.Marshal(args)
	m := map[string]mcpcap.Citation{}
	for _, c := range cites {
		m[c.Ref] = c
	}
	for _, t := range tools {
		if t.tool.Name == name {
			x.mu.Lock()
			defer x.mu.Unlock()
			return t.handler(&call{x: x, u: x.user("bl", "u"), businessLine: "bl", endUser: "u", session: session, args: raw, cites: m})
		}
	}
	panic(name)
}

func commitment(kind, content string, refs ...string) map[string]any {
	return map[string]any{"kind": kind, "content": content, "refs": refs}
}

// 交谈按归属进入项目或待确认；承诺按被引用原话的说话人核对；每日整理之后为涉及的项目提交"整理 <日期>"；项目 Session
// 经 day_summary 取回本项目的交谈与未完成的承诺，set_status 更新现状，update_commitments 对账。
func TestExchangesCommitmentsAndCollect(t *testing.T) {
	biz := &fakeBusiness{inputs: map[string][]string{}, texts: map[string]string{}, done: make(chan struct{}, 8)}
	x := &Server{ctx: context.Background(), users: map[string]*user{}, Business: biz,
		Now: func() time.Time { return time.Date(2026, 10, 23, 22, 0, 0, 0, time.Local) }}
	x.Seed("bl", "u", SeedData{Projects: []Project{{ID: "P-0001", Name: "年度预算", Status: "财务部在汇总各部门预算"}}})
	wearer := mcpcap.Citation{Ref: "#2.1", Speaker: "wearer"}
	other := mcpcap.Citation{Ref: "#2.0", Speaker: "other", SpeakerKey: "liu"}

	if out := invoke(x, "", "find_projects", map[string]string{"query": "预算"}); !strings.HasPrefix(out, "P-0001 年度预算：财务部在汇总各部门预算（未完成的承诺 0 条）") {
		t.Fatalf("find_projects = %q", out)
	}
	out := invoke(x, "ses_day1", "file_exchange", map[string]any{"exchange": "x2.0", "title": "办公室装修", "summary": "地板定了浅色木纹",
		"project": map[string]string{"match": "new", "name": "新办公室装修"}, "decisions": []string{"浅色木纹"},
		"commitments": []any{commitment("mine", "发报价单", "#2.1"), commitment("theirs", "带样板", "#2.0"), commitment("mine", "发预算表", "#2.0")}},
		wearer, other)
	if !strings.Contains(out, "归入 "+NewProject+" 新办公室装修") || !strings.Contains(out, "1 条未核实") || !strings.Contains(out, "「发预算表」引用的原话里没有用户本人说的话") {
		t.Fatalf("file_exchange = %q", out)
	}
	invoke(x, "ses_day1", "file_exchange", map[string]any{"exchange": "x5.0", "title": "预算", "summary": "还差两个部门",
		"project": map[string]string{"match": "unsure", "id": "P-0001", "name": "部门预算"}})
	invoke(x, "ses_day1", "file_exchange", map[string]any{"exchange": "x7.0", "title": "午饭", "summary": "约了周五吃饭",
		"project": map[string]string{"match": "none"}, "commitments": []any{commitment("mine", "周五请吃饭", "#9.9")}})
	if out := invoke(x, "ses_day1", "file_exchange", map[string]any{"exchange": "x8.0", "title": "?", "summary": "?",
		"project": map[string]string{"match": "existing", "id": "P-9999"}}); !strings.Contains(out, "没有项目 P-9999") {
		t.Fatalf("unknown project = %q", out)
	}
	days := invoke(x, "ses_day1", "day_exchanges", map[string]any{})
	for _, want := range []string{"x2.0 办公室装修（归入 P-0101 新办公室装修）", "x5.0 预算（待用户确认：可能属于 P-0001）", "x7.0 午饭（不归项目）",
		"承诺：用户答应：周五请吃饭（未核实：引用的原话不存在）"} {
		if !strings.Contains(days, want) {
			t.Fatalf("day_exchanges lacks %q:\n%s", want, days)
		}
	}

	if out := invoke(x, "ses_day1", "file_day", map[string]any{"day": map[string]any{"headline": "装修定稿"},
		"awareness": map[string]any{"habits": []string{"答应时给出期限"}}}); !strings.Contains(out, "3 段交谈") {
		t.Fatalf("file_day = %q", out)
	}
	select {
	case <-biz.done:
	case <-time.After(5 * time.Second):
		t.Fatal("collect did not submit to the project")
	}
	st := x.State("bl", "u")
	reno := st.Projects[1]
	if reno.ID != NewProject || reno.Session == "" || biz.texts[reno.Session] != "整理 2026-10-23" || len(biz.sessions) != 1 {
		t.Fatalf("project = %+v, sessions %v, inputs %v", reno, biz.sessions, biz.texts)
	}

	sum := invoke(x, reno.Session, "day_summary", map[string]string{"date": "2026-10-23"})
	for _, want := range []string{"地板定了浅色木纹", "C-0001 用户答应：发报价单", "C-0002 对方答应用户：带样板", "还差两个部门"} {
		if strings.Contains(sum, want) != (want != "还差两个部门") {
			t.Fatalf("day_summary (%q) =\n%s", want, sum)
		}
	}
	invoke(x, reno.Session, "set_status", map[string]any{"status": "设计定稿，等报价", "next_steps": []string{"发报价单"}})
	if out := invoke(x, reno.Session, "update_commitments", map[string]any{"done": []string{"C-0002"}, "cancelled": []string{"C-0004"}}); !strings.Contains(out, "不是本项目的承诺：C-0004") {
		t.Fatalf("update_commitments = %q", out)
	}
	if d := invoke(x, "", "project_digest", map[string]string{"project": NewProject}); !strings.Contains(d, "设计定稿，等报价（未完成的承诺 2 条）") || strings.Contains(d, "带样板") {
		t.Fatalf("project_digest = %q", d)
	}

	// 用户确认待定的交谈：按交谈给出的名称新建项目。
	if err := x.Resolve("bl", "u", "ses_day1", "x5.0", ""); err != nil {
		t.Fatal(err)
	}
	if err := x.Resolve("bl", "u", "ses_day1", "x5.0", ""); err == nil {
		t.Fatal("resolved twice")
	}
	if st := x.State("bl", "u"); st.Projects[2].Name != "部门预算" || st.Exchanges[1].Project != "P-0102" {
		t.Fatalf("after resolve: %+v", st)
	}

	// 同一段交谈重交：取代之前的结果与承诺，沿用上次新建的项目。
	invoke(x, "ses_day1", "file_exchange", map[string]any{"exchange": "x2.0", "title": "办公室装修", "summary": "重交",
		"project": map[string]string{"match": "new", "name": "新办公室装修"}, "commitments": []any{commitment("mine", "发报价单", "#2.1")}}, wearer)
	st = x.State("bl", "u")
	n := 0
	for _, m := range st.Commitments {
		if m.Exchange == "x2.0" {
			n++
		}
	}
	if len(st.Projects) != 3 || n != 1 || len(st.Exchanges) != 3 {
		t.Fatalf("after refiling: %d projects, %d commitments of x2.0, %d exchanges", len(st.Projects), n, len(st.Exchanges))
	}
	// 另一个 EndUser 看不到。
	if st := x.State("bl", "v"); len(st.Days) != 0 || len(st.Projects) != 0 || len(st.Commitments) != 0 {
		t.Fatalf("other end user sees %+v", st)
	}
}

// 每周回顾：归档 30 天没有新交谈、也没有未完成承诺的项目；合并提议由用户确认后移动交谈与承诺；file_week 记下本周觉察；
// Week 为本周创建一次回顾的 Session，并向本周有交谈的项目提交"重写现状"。
func TestWeeklyReview(t *testing.T) {
	biz := &fakeBusiness{inputs: map[string][]string{}, texts: map[string]string{}, done: make(chan struct{}, 8)}
	now := time.Date(2026, 10, 25, 22, 0, 0, 0, time.Local)
	x := &Server{ctx: context.Background(), users: map[string]*user{}, Business: biz, Now: func() time.Time { return now }}
	x.Seed("bl", "u", SeedData{
		Projects: []Project{{ID: "P-0001", Name: "新办公室装修", Status: "地板定了"}, {ID: "P-0002", Name: "办公室搬迁", Status: "十二月搬"},
			{ID: "P-0003", Name: "旧事", Status: "早就结束了"}, {ID: "P-0004", Name: "旧事但有承诺", Status: "还欠一件事"}},
		Exchanges: []Exchange{{Date: "2026-10-24", Title: "看样板", Project: "P-0001"}, {Date: "2026-10-23", Title: "定搬家公司", Project: "P-0002"},
			{Date: "2026-08-01", Title: "收尾", Project: "P-0003"}, {Date: "2026-08-01", Title: "收尾", Project: "P-0004"}},
		Commitments: []Commitment{{Kind: "mine", Content: "寄发票", Project: "P-0004"}, {Kind: "theirs", Content: "报价", Project: "P-0002"}},
		Filed: []Day{{Date: "2026-10-24", Headline: "看样板", Awareness: Awareness{Habits: []string{"答应时给出期限"}}},
			{Date: "2026-10-10", Headline: "上上周", Awareness: Awareness{Habits: []string{"太早了"}}}},
	})
	x.Bind("bl", "u", "P-0001", "ses_p1")
	if got := x.Archive("bl", "u"); len(got) != 1 || got[0] != "P-0003" {
		t.Fatalf("archived %v", got)
	}
	if out := invoke(x, "", "find_projects", map[string]string{"query": "旧事"}); strings.Contains(out, "P-0003") || !strings.Contains(out, "P-0004") {
		t.Fatalf("find_projects after archive = %q", out)
	}
	if out := invoke(x, "ses_w", "week_overview", map[string]any{}); !strings.Contains(out, "2026-10-24：看样板") || strings.Contains(out, "上上周") ||
		!strings.Contains(out, "沟通习惯：答应时给出期限") {
		t.Fatalf("week_overview = %q", out)
	}
	if out := invoke(x, "ses_w", "similar_projects", map[string]any{}); !strings.Contains(out, "第 1 对") || strings.Contains(out, "P-0003") {
		t.Fatalf("similar_projects = %q", out)
	}
	if out := invoke(x, "ses_w", "propose_merge", map[string]string{"into": "P-0001", "from": "P-0002", "reason": "同一次搬迁"}); !strings.Contains(out, "M-0001") {
		t.Fatalf("propose_merge = %q", out)
	}
	if out := invoke(x, "ses_w", "propose_merge", map[string]string{"into": "P-0002", "from": "P-0001", "reason": "重试"}); !strings.Contains(out, "已经提议过") {
		t.Fatalf("repeated propose_merge = %q", out)
	}
	if err := x.ResolveMerge("bl", "u", "M-0001", true); err != nil {
		t.Fatal(err)
	}
	st := x.State("bl", "u")
	if st.Projects[1].MergedInto != "P-0001" || st.Exchanges[1].Project != "P-0001" || st.Commitments[1].Project != "P-0001" || st.Merges[0].Status != "accepted" {
		t.Fatalf("after merge: %+v", st)
	}
	if out := invoke(x, "ses_w", "file_week", map[string]any{"habits": []map[string]any{{"habit": "给出期限", "days": []string{"2026-10-21", "2026-10-24"}},
		{"habit": "发纪要", "days": []string{"2026-10-22", "2026-10-22"}}}}); !strings.Contains(out, "没有收录：发纪要") || len(x.State("bl", "u").Weeks[0].Habits) != 1 {
		t.Fatalf("file_week = %q", out)
	}

	sid, err := x.Week(context.Background(), "bl", "u")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		<-biz.done
	}
	again, _ := x.Week(context.Background(), "bl", "u")
	if again != sid || biz.texts[sid] != "回顾本周" || biz.texts["ses_p1"] != "重写现状" || len(biz.inputs[sid]) != 1 {
		t.Fatalf("week sessions %s %s, inputs %v", sid, again, biz.texts)
	}
}
