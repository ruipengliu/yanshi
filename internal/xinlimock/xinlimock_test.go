package xinlimock

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
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
	if agent != ProjectAgent {
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

func invoke(x *Server, session, name string, args any) string {
	raw, _ := json.Marshal(args)
	for _, t := range tools {
		if t.tool.Name == name {
			x.mu.Lock()
			defer x.mu.Unlock()
			return t.handler(&call{x: x, u: x.user("bl", "u"), businessLine: "bl", endUser: "u", session: session, args: raw})
		}
	}
	panic(name)
}

// 每日整理按今日、项目、觉察存档；扮演业务线服务端为涉及的项目创建 xinli-project Session 并提交"整理 <日期>"；
// 项目 Session 经 day_summary 取回本项目的分组，以 set_commitments 交出的承诺记在项目上。
func TestFileDayCollectsIntoProjects(t *testing.T) {
	biz := &fakeBusiness{inputs: map[string][]string{}, texts: map[string]string{}, done: make(chan struct{}, 8)}
	x := &Server{ctx: context.Background(), users: map[string]*user{}, Business: biz,
		Now: func() time.Time { return time.Date(2026, 10, 23, 22, 0, 0, 0, time.Local) }}
	x.Seed("bl", "u", []Project{{ID: "P-0001", Name: "年度预算", Digest: "汇总中"}}, nil)

	out := invoke(x, "ses_day1", "file_day", map[string]any{
		"day": map[string]any{"headline": "装修定稿，预算推进", "highlights": []string{"地板定了浅色木纹"}},
		"groups": []map[string]any{
			{"title": "办公室装修", "summary": "地板定了", "decisions": []string{"浅色木纹"}, "my_todos": []string{"周五前发报价单"},
				"their_todos": []string{"刘工周四带样板"}, "risks": []string{"家具可能延期"}},
			{"project": "P-0001", "title": "年度预算", "summary": "还差两个部门"},
		},
		"awareness": map[string]any{"habits": []string{"答应时给出期限"}, "suggestions": []string{"会后发一份纪要"}},
	})
	if !strings.Contains(out, "已新建项目 "+NewProject+" 办公室装修") || !strings.Contains(out, "已归入 P-0001 年度预算") {
		t.Fatalf("file_day = %q", out)
	}
	for range 2 {
		select {
		case <-biz.done:
		case <-time.After(5 * time.Second):
			t.Fatal("collect did not submit to both projects")
		}
	}
	st := x.State("bl", "u")
	if len(st.Days) != 1 || st.Days[0].Headline != "装修定稿，预算推进" || st.Days[0].Awareness.Suggestions[0] != "会后发一份纪要" {
		t.Fatalf("days = %+v", st.Days)
	}
	var reno *Project
	for i := range st.Projects {
		if st.Projects[i].ID == NewProject {
			reno = &st.Projects[i]
		}
	}
	if reno == nil || reno.Session == "" || len(reno.Timeline) != 1 || biz.texts[reno.Session] != "整理 2026-10-23" {
		t.Fatalf("project = %+v, inputs %v", reno, biz.texts)
	}

	sum := invoke(x, reno.Session, "day_summary", map[string]string{"date": "2026-10-23"})
	if !strings.Contains(sum, "用户本人答应：周五前发报价单") || !strings.Contains(sum, "对方答应：刘工周四带样板") || strings.Contains(sum, "还差两个部门") {
		t.Fatalf("day_summary = %q", sum)
	}
	invoke(x, reno.Session, "set_commitments", map[string]any{"commitments": []map[string]string{{"content": "发报价单", "to": "刘工", "due": "周五"}}})
	if d := invoke(x, "", "project_digest", map[string]string{"project": NewProject}); !strings.Contains(d, "发报价单（答应 刘工），期限 周五") {
		t.Fatalf("project_digest = %q", d)
	}

	// 同一 Session 重试整理：取代当天的结果；归集的输入 ID 不变，项目 Session 不重复收到。
	invoke(x, "ses_day1", "file_day", map[string]any{"day": map[string]any{"headline": "重试"},
		"groups": []map[string]any{{"project": NewProject, "title": "办公室装修", "summary": "地板定了"}}})
	time.Sleep(50 * time.Millisecond)
	if st := x.State("bl", "u"); len(st.Days) != 1 || st.Days[0].Headline != "重试" || len(biz.inputs[reno.Session]) != 1 {
		t.Fatalf("after retry: days %+v, inputs %v", st.Days, biz.inputs)
	}
	// 没有分组时也有确认。
	if out := invoke(x, "ses_day2", "file_day", map[string]any{"day": map[string]any{"headline": "闲聊"}, "groups": []any{}}); !strings.Contains(out, "已收到") {
		t.Fatalf("file_day without groups = %q", out)
	}
	// 另一个 EndUser 看不到。
	if st := x.State("bl", "v"); len(st.Days) != 0 || len(st.Projects) != 0 {
		t.Fatalf("other end user sees %+v", st)
	}
}
