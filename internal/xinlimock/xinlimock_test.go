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

var shanghai, _ = time.LoadLocation("Asia/Shanghai")

// said 是平台对一个引用的解析：说话人与说话时间（2026-10-23 星期五的当地时刻）。
func said(ref, speaker, hhmm string) mcpcap.Citation {
	t, _ := time.ParseInLocation("2006-01-02 15:04", "2026-10-23 "+hhmm, shanghai)
	c := mcpcap.Citation{Ref: ref, Speaker: speaker, SpokenAt: t.UTC(), TimeZone: "Asia/Shanghai"}
	if speaker == "other" {
		c.SpeakerKey = "s1"
	}
	return c
}

func newServer(biz Business) *Server {
	return &Server{ctx: context.Background(), users: map[string]*user{}, Business: biz,
		Now: func() time.Time { return time.Date(2026, 10, 23, 22, 0, 0, 0, shanghai) }}
}

type M = map[string]any

func span(from, to string) M { return M{"from": from, "to": to} }

func exchange(from, to, title, summary string, projects ...M) M {
	return M{"from": from, "to": to, "title": title, "summary": summary, "projects": projects}
}

func create(ex, kind, content string, refs ...string) M {
	return M{"exchange": ex, "kind": kind, "content": content, "refs": refs}
}

func changes(source M, exchanges []M, commitments M) M {
	return M{"source": source, "exchanges": M{"create": exchanges}, "commitments": commitments}
}

func contains(t *testing.T, label, got string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Fatalf("%s lacks %q:\n%s", label, w, got)
		}
	}
}

func excludes(t *testing.T, label, got string, nots ...string) {
	t.Helper()
	for _, w := range nots {
		if strings.Contains(got, w) {
			t.Fatalf("%s contains %q:\n%s", label, w, got)
		}
	}
}

// 一个时段里划出几段交谈，一个变更集交出：归属进入项目或待确认，承诺按被引用原话的说话人核对；每日整理之后为涉及的项目
// 提交"整理 <日期>"；项目 Session 经 day_summary 取回本项目的交谈与未完成的承诺，set_status 更新现状。同一个来源重交时
// 整体取代，沿用新建的项目与承诺的 ID。
func TestChangesetsAndCollect(t *testing.T) {
	biz := &fakeBusiness{inputs: map[string][]string{}, texts: map[string]string{}, done: make(chan struct{}, 8)}
	x := newServer(biz)
	x.Seed("bl", "u", SeedData{Projects: []Project{{ID: "P-0001", Name: "年度预算", Status: "财务部在汇总各部门预算"}}})
	cites := []mcpcap.Citation{said("#2.0", "other", "09:00"), said("#2.1", "wearer", "09:01"), said("#2.2", "other", "09:02"),
		said("#2.5", "wearer", "09:20"), said("#2.6", "other", "09:21"), said("#2.8", "wearer", "09:40"), said("#2.9", "wearer", "09:41")}

	if out := invoke(x, "", "find_related", M{"query": "预算"}); !strings.Contains(out, "P-0001 年度预算：财务部在汇总各部门预算（未完成的承诺 0 条）") ||
		!strings.Contains(out, "相关的未完成承诺：没有") {
		t.Fatalf("find_related = %q", out)
	}
	cs := changes(span("#2.0", "#2.9"), []M{
		exchange("#2.0", "#2.2", "办公室装修", "地板定了浅色木纹", M{"match": "new", "name": "新办公室装修"}),
		exchange("#2.5", "#2.6", "预算", "还差两个部门", M{"match": "unsure", "id": "P-0001", "name": "部门预算"}),
		exchange("#2.8", "#2.9", "午饭", "约了周五吃饭", M{"match": "none"}),
	}, M{"create": []M{create("x2.0", "mine", "发报价单", "#2.1"), create("x2.0", "theirs", "带样板", "#2.0"),
		create("x2.0", "mine", "发预算表", "#2.0"), create("x2.8", "mine", "周五请吃饭", "#2.9")}})
	out := invoke(x, "ses_day1", "file_changes", cs, cites...)
	contains(t, "file_changes", out, "交谈 x2.0「办公室装修」：归入 "+NewProject+" 新办公室装修", "交谈 x2.5「预算」：待用户确认：可能属于 P-0001",
		"交谈 x2.8「午饭」：不归项目", "C-0003（未核实：引用的原话里没有用户本人说的话", "1 条承诺未核实")
	ledger := invoke(x, "ses_day1", "day_ledger", M{})
	contains(t, "day_ledger", ledger, "x2.0（#2.0–#2.2）办公室装修［归入 P-0101 新办公室装修］", "x2.5（#2.5–#2.6）预算［待用户确认：可能属于 P-0001］",
		"C-0004（rev 1）用户答应：周五请吃饭；出处 x2.8")
	if out := invoke(x, "ses_day1", "file_day", M{"day": M{"headline": "装修定稿"}}); !strings.Contains(out, "3 段交谈") {
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
	sum := invoke(x, reno.Session, "day_summary", M{"date": "2026-10-23"})
	contains(t, "day_summary", sum, "交谈 x2.0 办公室装修", "地板定了浅色木纹", "C-0001（rev 1）用户答应：发报价单", "C-0002（rev 1）对方答应用户：带样板")
	excludes(t, "day_summary", sum, "还差两个部门")
	invoke(x, reno.Session, "set_status", M{"status": "设计定稿，等报价", "next_steps": []string{"发报价单"}})
	if d := invoke(x, "", "project_digest", M{"project": NewProject}); !strings.Contains(d, "设计定稿，等报价（未完成的承诺 3 条）") {
		t.Fatalf("project_digest = %q", d)
	}

	// 用户确认待定的交谈：按交谈给出的名称新建项目，记为用户的变更集。
	if err := x.Resolve("bl", "u", "ses_day1", "x2.5", ""); err != nil {
		t.Fatal(err)
	}
	if err := x.Resolve("bl", "u", "ses_day1", "x2.5", ""); err == nil {
		t.Fatal("resolved twice")
	}
	if st := x.State("bl", "u"); st.Projects[2].Name != "部门预算" || st.Exchanges[1].InProjects()[0] != "P-0102" {
		t.Fatalf("after resolve: %+v", st)
	}

	// 同一个来源重交（重试、接管）：取代之前的结果，沿用新建的项目与承诺的 ID，不重复。
	cs["commitments"] = M{"create": []M{create("x2.0", "mine", "发报价单", "#2.1")}}
	cs["exchanges"] = M{"create": []M{exchange("#2.0", "#2.2", "办公室装修", "重交", M{"match": "new", "name": "新办公室装修"})}}
	if out := invoke(x, "ses_day1", "file_changes", cs, cites...); !strings.Contains(out, "承诺 C-0001（已核实）") || !strings.Contains(out, "均已核实") {
		t.Fatalf("refiling = %q", out)
	}
	st = x.State("bl", "u")
	if len(st.Projects) != 3 || len(st.Commitments) != 1 || len(st.Exchanges) != 1 || st.Exchanges[0].Summary != "重交" {
		t.Fatalf("after refiling: %d projects, %+v commitments, %+v exchanges", len(st.Projects), st.Commitments, st.Exchanges)
	}
	// 另一个 EndUser 看不到。
	if st := x.State("bl", "v"); len(st.Days) != 0 || len(st.Projects) != 0 || len(st.Commitments) != 0 {
		t.Fatalf("other end user sees %+v", st)
	}
}

// 重交改变创建项的位置时，之后的修订和用户锁定仍须属于原来的承诺。
func TestRefileCommitmentIdentity(t *testing.T) {
	for _, name := range []string{"same", "reorder", "delete_first", "insert_first"} {
		t.Run(name, func(t *testing.T) {
			x := newServer(nil)
			cites := []mcpcap.Citation{said("#2.0", "wearer", "09:00"), said("#2.1", "wearer", "09:01"), said("#2.2", "wearer", "09:02")}
			a, b, c := create("x2.0", "mine", "发报价", "#2.0"), create("x2.0", "mine", "带样板", "#2.1"), create("x2.0", "mine", "寄合同", "#2.2")
			file := func(items []M) string {
				return invoke(x, "ses", "file_changes", changes(span("#2.0", "#2.2"), []M{exchange("#2.0", "#2.2", "装修", "商定安排", M{"match": "none"})}, M{"create": items}), cites...)
			}
			contains(t, "initial", file([]M{a, b}), "已收下")
			contains(t, "revise", invoke(x, "ses", "file_changes", changes(span("#9.0", "#9.0"), nil, M{"revise": []M{{"id": "C-0002", "rev": 1, "due_date": "2026-10-28", "refs": []string{"#9.0"}}}}), said("#9.0", "wearer", "15:00")), "已收下")
			if err := x.Edit("bl", "u", "C-0002", "done", ""); err != nil {
				t.Fatal(err)
			}
			items := []M{a, b}
			switch name {
			case "reorder":
				items = []M{b, a}
			case "delete_first":
				items = []M{b}
			case "insert_first":
				items = []M{c, a, b}
			}
			contains(t, "refile", file(items), "已收下")
			st := x.State("bl", "u")
			if len(st.Commitments) != len(items) {
				t.Fatalf("commitments = %+v", st.Commitments)
			}
			for _, m := range st.Commitments {
				if m.Content == "带样板" {
					if m.ID != "C-0002" || m.Status != "done" || m.DueDate != "2026-10-28" || m.Rev() != 3 || len(m.Locked) != 1 || m.Locked[0] != "status" {
						t.Fatalf("identity changed: %+v", m)
					}
				} else if m.Status != "open" || m.Rev() != 1 {
					t.Fatalf("references moved: %+v", m)
				}
			}
		})
	}
}

// 对应不上且已有引用时必须整体拒收；没有引用的旧承诺可以撤去。
func TestRefileCommitmentReferences(t *testing.T) {
	for _, reference := range []string{"user", "revise", "close", "merge_from", "merge_into", "none"} {
		for _, replacement := range []string{"delete", "kind", "split", "combine"} {
			t.Run(reference+"/"+replacement, func(t *testing.T) {
				x := newServer(nil)
				cites := []mcpcap.Citation{said("#2.0", "wearer", "09:00"), said("#2.1", "wearer", "09:01")}
				a, b := create("x2.0", "mine", "发报价", "#2.0"), create("x2.0", "mine", "带样板", "#2.1")
				file := func(items []M, project M) string {
					return invoke(x, "ses", "file_changes", changes(span("#2.0", "#2.1"), []M{exchange("#2.0", "#2.1", "装修", "商定安排", project)}, M{"create": items}), cites...)
				}
				contains(t, "initial", file([]M{a, b}, M{"match": "none"}), "已收下")
				ops := M{}
				switch reference {
				case "user":
					if err := x.Edit("bl", "u", "C-0001", "done", ""); err != nil {
						t.Fatal(err)
					}
				case "revise":
					ops["revise"] = []M{{"id": "C-0001", "rev": 1, "due_date": "2026-10-28", "refs": []string{"#2.0"}}}
				case "close":
					ops["close"] = []M{{"id": "C-0001", "rev": 1, "status": "done", "refs": []string{"#2.0"}}}
				case "merge_from":
					ops["merge"] = []M{{"into": "C-0002", "from": []string{"C-0001"}}}
				case "merge_into":
					ops["merge"] = []M{{"into": "C-0001", "from": []string{"C-0002"}}}
				}
				if len(ops) > 0 {
					contains(t, "reference", invoke(x, "ses", "file_changes", changes(M{"day": true}, nil, ops), cites...), "已收下")
				}
				before, _ := json.Marshal(x.State("bl", "u"))
				items := []M{b}
				switch replacement {
				case "kind":
					items = append(items, create("x2.0", "theirs", "发报价", "#2.0"))
				case "split":
					items = append(items, a, create("x2.0", "mine", "补报价", "#2.0"))
				case "combine":
					items = []M{create("x2.0", "mine", "发报价并带样板", "#2.0", "#2.1")}
				}
				out := file(items, M{"match": "new", "name": "新装修项目"})
				if reference != "none" {
					contains(t, "reject", out, "没有被接受", "C-0001", "发报价", "#2.0", "id", "重交")
					after, _ := json.Marshal(x.State("bl", "u"))
					if string(before) != string(after) {
						t.Fatal("rejected changeset changed state")
					}
					contains(t, "retry", file([]M{b, a}, M{"match": "none"}), "已收下")
				} else {
					contains(t, "unreferenced", out, "已收下")
					for _, m := range x.State("bl", "u").Commitments {
						if m.ID == "C-0001" {
							t.Fatalf("ambiguous identity reused: %+v", m)
						}
						if replacement == "combine" && m.ID == "C-0002" {
							t.Fatalf("many-to-one identity reused: %+v", m)
						}
					}
				}
			})
		}
	}
}

func TestCreateCommitmentTemporaryID(t *testing.T) {
	for _, id := range []string{"c1", "quote", "1", "#local", "C-", "C-quote", "C-1x", "C-１２"} {
		t.Run(id, func(t *testing.T) {
			x := newServer(nil)
			m := create("x2.0", "mine", "发报价", "#2.0")
			m["id"] = id
			args := changes(span("#2.0", "#2.0"), []M{exchange("#2.0", "#2.0", "报价", "商定安排", M{"match": "none"})}, M{"create": []M{m}})
			file := func() string { return invoke(x, "ses", "file_changes", args, said("#2.0", "wearer", "09:00")) }
			contains(t, "create", file(), "已收下", "C-0001")
			if err := x.Edit("bl", "u", "C-0001", "done", ""); err != nil {
				t.Fatal(err)
			}
			contains(t, "refile", file(), "已收下", "C-0001")
			st := x.State("bl", "u")
			if len(st.Commitments) != 1 || st.Commitments[0].ID != "C-0001" || st.Commitments[0].Status != "done" {
				t.Fatalf("temporary id changed identity: %+v", st.Commitments)
			}
		})
	}
}

func TestCreateCommitmentForeignID(t *testing.T) {
	x := newServer(nil)
	m := create("x2.0", "mine", "发报价", "#2.0")
	args := changes(span("#2.0", "#2.0"), []M{exchange("#2.0", "#2.0", "报价", "商定安排", M{"match": "none"})}, M{"create": []M{m}})
	contains(t, "initial", invoke(x, "ses", "file_changes", args, said("#2.0", "wearer", "09:00")), "已收下")
	before, _ := json.Marshal(x.State("bl", "u"))
	m = create("x9.0", "mine", "带样板", "#9.0")
	m["id"] = "C-0001"
	out := invoke(x, "ses", "file_changes", changes(span("#9.0", "#9.0"), []M{exchange("#9.0", "#9.0", "样板", "商定安排", M{"match": "none"})}, M{"create": []M{m}}), said("#9.0", "wearer", "15:00"))
	contains(t, "foreign id", out, "没有被接受", "C-0001", "不是这个 source 上次新建的承诺")
	after, _ := json.Marshal(x.State("bl", "u"))
	if string(before) != string(after) {
		t.Fatal("foreign id changed state")
	}
}

func TestRefileExplicitCommitmentID(t *testing.T) {
	for _, mode := range []string{"explicit", "priority", "duplicate", "foreign", "temporary", "intersection"} {
		t.Run(mode, func(t *testing.T) {
			x := newServer(nil)
			cites := []mcpcap.Citation{said("#2.0", "wearer", "09:00"), said("#2.1", "wearer", "09:01"), said("#2.2", "wearer", "09:02")}
			file := func(items []M) string {
				return invoke(x, "ses", "file_changes", changes(span("#2.0", "#2.2"), []M{exchange("#2.0", "#2.2", "装修", "商定安排", M{"match": "none"})}, M{"create": items}), cites...)
			}
			a, b := create("x2.0", "mine", "发报价", "#2.0"), create("x2.0", "mine", "带样板", "#2.1")
			contains(t, "initial", file([]M{a, b}), "已收下")
			if err := x.Edit("bl", "u", "C-0001", "done", ""); err != nil {
				t.Fatal(err)
			}
			a["id"] = "C-0001"
			a["refs"] = []string{"#2.2"}
			switch mode {
			case "priority":
				a["refs"] = []string{"#2.1"}
			case "duplicate":
				b["id"] = "C-0001"
			case "foreign":
				a["id"] = "C-9999"
			case "temporary":
				a["id"] = "#local"
				a["refs"] = []string{"#2.0"}
			case "intersection":
				delete(a, "id")
				a["refs"] = []string{"#2.2", "#2.0"}
			}
			before, _ := json.Marshal(x.State("bl", "u"))
			out := file([]M{b, a})
			if mode == "duplicate" || mode == "foreign" {
				contains(t, "invalid id", out, "没有被接受")
				if mode == "duplicate" {
					contains(t, "previous identity", out, "C-0001", "发报价", "#2.0", "上次创建项对应")
				}
				after, _ := json.Marshal(x.State("bl", "u"))
				if string(before) != string(after) {
					t.Fatal("invalid id changed state")
				}
				return
			}
			contains(t, "refile", out, "已收下")
			for _, m := range x.State("bl", "u").Commitments {
				if m.Content == "发报价" && (m.ID != "C-0001" || m.Status != "done") {
					t.Fatalf("explicit identity lost: %+v", m)
				}
				if m.Content == "带样板" && (m.ID != "C-0002" || m.Status != "open") {
					t.Fatalf("other identity changed: %+v", m)
				}
			}
		})
	}
}

// 上午答应、下午改期：下午那次白天整理依据新说出的原话修订同一条承诺，只剩一条 open、两条修订；改由他人去做的作废。
// 白天引用旧原话的修订、过时的 rev 被整体拒绝；用户在 App 中改过的字段，模型的修订被忽略并说明。
func TestRevisionsAcrossChangesets(t *testing.T) {
	x := newServer(nil)
	am := []mcpcap.Citation{said("#2.0", "other", "10:39"), said("#2.1", "wearer", "10:40"), said("#2.2", "wearer", "10:41")}
	out := invoke(x, "ses", "file_changes", changes(span("#2.0", "#2.2"), []M{exchange("#2.0", "#2.2", "康宁验收", "定了验收流程", M{"match": "none"})},
		M{"create": []M{
			{"exchange": "x2.0", "kind": "mine", "content": "给张总发验收方案", "counterpart": "张总", "due_text": "下周三前", "due_date": "2026-10-28", "refs": []string{"#2.1"}},
			{"exchange": "x2.0", "kind": "mine", "content": "周六送孩子去钢琴课", "counterpart": "爱人", "due_text": "周六", "refs": []string{"#2.2"}},
		}}), am...)
	contains(t, "morning", out, "C-0001（已核实）", "C-0002（已核实）用户答应爱人：周六送孩子去钢琴课，期限 2026-10-24（周六）")

	related := invoke(x, "ses", "find_related", M{"query": "验收方案", "counterparts": []string{"张总"}})
	contains(t, "find_related", related, "C-0001（rev 1）用户答应张总：给张总发验收方案，期限 2026-10-28（下周三前）")

	pm := []mcpcap.Citation{said("#9.0", "other", "15:02"), said("#9.1", "wearer", "15:03"), said("#9.2", "wearer", "15:05")}
	afternoon := func(revise []M) M {
		return changes(span("#9.0", "#9.2"), []M{exchange("#9.0", "#9.2", "张总来电", "验收提前", M{"match": "none"})},
			M{"revise": revise, "close": []M{{"id": "C-0002", "rev": 1, "status": "cancelled", "refs": []string{"#9.2"}, "reason": "改由爱人送"}}})
	}
	// 日期按说话那天换算：周五说的"下周二"是 10-27。
	if out := invoke(x, "ses", "file_changes", afternoon([]M{{"id": "C-0001", "rev": 1, "due_text": "下周二", "due_date": "2026-10-28", "refs": []string{"#9.1"}}}), pm...); !strings.Contains(out, "没有被接受") ||
		!strings.Contains(out, "期限「下周二」按说话那天（2026-10-23 星期五）换算是 2026-10-27") {
		t.Fatalf("wrong date = %q", out)
	}
	// 白天只能依据本次读到的原话。
	if out := invoke(x, "ses", "file_changes", afternoon([]M{{"id": "C-0001", "rev": 1, "due_text": "下周二", "refs": []string{"#2.1"}}}), append(pm, am...)...); !strings.Contains(out, "#2.1 不在本次读到的范围 #9.0–#9.2 内") {
		t.Fatalf("old ref = %q", out)
	}
	out = invoke(x, "ses", "file_changes", afternoon([]M{{"id": "C-0001", "rev": 1, "due_text": "下周二", "refs": []string{"#9.1"}, "reason": "张总要求提前"}}), pm...)
	contains(t, "afternoon", out, "修订 C-0001，现在 rev 2：用户答应张总：给张总发验收方案，期限 2026-10-27（下周二）", "关闭 C-0002（cancelled）")
	st := x.State("bl", "u")
	c1 := st.Commitments[0]
	if len(st.Commitments) != 2 || !c1.Open() || c1.Rev() != 2 || st.Commitments[1].Status != "cancelled" ||
		c1.Revisions[1].SpokenAt.In(shanghai).Format("15:04") != "15:03" || c1.Revisions[1].Basis() != "原话" {
		t.Fatalf("ledger = %+v", st.Commitments)
	}
	// 重交同一个来源时，rev 对着没有旧变更集的账本检查：照样通过，不叠加两次。
	invoke(x, "ses", "file_changes", afternoon([]M{{"id": "C-0001", "rev": 1, "due_text": "下周二", "refs": []string{"#9.1"}}}), pm...)
	if c := x.State("bl", "u").Commitments[0]; c.Rev() != 2 {
		t.Fatalf("refiled revision applied twice: %+v", c.Revisions)
	}

	// 用户在 App 中改了期限：模型按旧 rev 的修订被拒绝，附上现在的状态；按新 rev 再交，期限以用户的为准。
	if err := x.Edit("bl", "u", "C-0001", "", "2026-10-26"); err != nil {
		t.Fatal(err)
	}
	pm2 := []mcpcap.Citation{said("#12.0", "wearer", "16:00"), said("#12.1", "wearer", "16:01")}
	late := func(rev int) M {
		return changes(span("#12.0", "#12.1"), nil, M{"revise": []M{{"id": "C-0001", "rev": rev, "due_text": "下周三", "content": "给张总发验收方案与报价", "refs": []string{"#12.1"}}}})
	}
	if out := invoke(x, "ses", "file_changes", late(2), pm2...); !strings.Contains(out, "C-0001 的 rev 是 3 而不是 2") || !strings.Contains(out, "期限 2026-10-26") {
		t.Fatalf("stale rev = %q", out)
	}
	out = invoke(x, "ses", "file_changes", late(3), pm2...)
	contains(t, "user priority", out, "用户在 App 中改过期限", "现在 rev 4：用户答应张总：给张总发验收方案与报价，期限 2026-10-26")
}

// 当天整理：白天的变更集不能合并；当天整理依据 day_ledger 的提示合并被静默切开的交谈与重复的承诺，被合并的保留、
// 标明并入哪一条，不参与召回。
func TestDayConsolidation(t *testing.T) {
	x := newServer(nil)
	x.Seed("bl", "u", SeedData{Projects: []Project{{ID: "P-0001", Name: "康宁交付"}}})
	c1 := []mcpcap.Citation{said("#2.0", "other", "10:30"), said("#2.1", "wearer", "10:56")}
	c2 := []mcpcap.Citation{said("#5.0", "other", "11:05"), said("#5.1", "wearer", "11:20")}
	invoke(x, "ses", "file_changes", changes(span("#2.0", "#2.1"), []M{exchange("#2.0", "#2.1", "康宁会议（上）", "试点进度", M{"match": "existing", "id": "P-0001"})},
		M{"create": []M{{"exchange": "x2.0", "kind": "mine", "content": "整理试点门店的问题清单发给李经理", "counterpart": "李经理", "refs": []string{"#2.1"}}}}), c1...)
	if out := invoke(x, "ses", "file_changes", M{"source": span("#5.0", "#5.1"), "exchanges": M{"merge": []M{{"into": "x2.0", "from": []string{"x5.0"}}}}}, c2...); !strings.Contains(out, "只在「整理今天」的当天整理中做") {
		t.Fatalf("daytime merge = %q", out)
	}
	// 写了范围的按范围交出，即使也写了 day: true；只有新建时可以直接写成列表。
	if out := invoke(x, "ses", "file_changes", M{"source": M{"from": "#5.0", "to": "#5.1", "day": true},
		"exchanges":   []M{exchange("#5.0", "#5.1", "康宁会议（下）", "上线日期", M{"match": "existing", "id": "P-0001"})},
		"commitments": []M{{"exchange": "x5.0", "kind": "mine", "content": "把试点门店的问题清单整理好发李经理", "counterpart": "李经理", "refs": []string{"#5.1"}}}}, c2...); !strings.Contains(out, "（来源 #5.0–#5.1）") {
		t.Fatalf("lenient changes = %q", out)
	}
	ledger := invoke(x, "ses", "day_ledger", M{})
	contains(t, "day_ledger", ledger, "可能重复的承诺：C-0001 与 C-0002（同一对方「李经理」、内容相近）", "相邻且归属相同的交谈：x2.0 与 x5.0（都归入 P-0001）")

	day := M{"source": M{"day": true}, "exchanges": M{"merge": []M{{"into": "x2.0", "from": []string{"x5.0"}, "title": "康宁会议", "summary": "试点进度与上线日期"}}},
		"commitments": M{"merge": []M{{"into": "C-0002", "from": []string{"C-0001"}, "reason": "同一件事"}}}}
	out := invoke(x, "ses", "file_changes", day)
	contains(t, "day changes", out, "已收下", "交谈 x5.0 并入 x2.0", "承诺 C-0001 并入 C-0002")
	st := x.State("bl", "u")
	ex := st.Exchanges[0]
	if !ex.Live() || ex.Title != "康宁会议" || ex.To != "#5.1" || st.Exchanges[1].MergedInto != "x2.0" || st.Commitments[0].MergedInto != "C-0002" || !st.Commitments[1].Open() {
		t.Fatalf("after consolidation: %+v", st)
	}
	if out := invoke(x, "ses", "find_related", M{"query": "问题清单", "counterparts": []string{"李经理"}}); strings.Contains(out, "C-0001") || !strings.Contains(out, "C-0002") {
		t.Fatalf("find_related after merge = %q", out)
	}
	// 重交当天整理：整体取代，合并不叠加两次。
	invoke(x, "ses", "file_changes", day)
	if c := x.State("bl", "u").Commitments[0]; c.Rev() != 2 || c.MergedInto != "C-0002" {
		t.Fatalf("refiled day changes: %+v", c)
	}
}

// 变更集整体检查：范围重叠、健康信息、涉及多个项目而承诺不写 project、引用不存在，都整体拒绝、什么都不记下；期限说法能
// 换算出唯一日期而 due_date 为空时补上。
func TestChangesetChecks(t *testing.T) {
	x := newServer(nil)
	x.Seed("bl", "u", SeedData{Projects: []Project{{ID: "P-0001", Name: "OKR 汇报"}, {ID: "P-0002", Name: "康宁交付"}}})
	cites := []mcpcap.Citation{said("#3.0", "other", "15:40"), said("#3.1", "wearer", "15:41"), said("#3.2", "wearer", "15:42")}
	reject := func(label string, args M, want string) {
		t.Helper()
		out := invoke(x, "ses", "file_changes", args, cites...)
		if !strings.Contains(out, "没有被接受") || !strings.Contains(out, want) {
			t.Fatalf("%s = %q", label, out)
		}
		if st := x.State("bl", "u"); len(st.Changesets) != 0 {
			t.Fatalf("%s: recorded %d changesets", label, len(st.Changesets))
		}
	}
	both := []M{{"match": "existing", "id": "P-0001"}, {"match": "existing", "id": "P-0002"}}
	reject("overlap", changes(span("#3.0", "#3.2"), []M{exchange("#3.0", "#3.1", "a", "a", M{"match": "none"}), exchange("#3.1", "#3.2", "b", "b", M{"match": "none"})}, nil), "重叠")
	reject("health", changes(span("#3.0", "#3.2"), []M{exchange("#3.0", "#3.2", "黄总谈话", "老陈最近失眠", M{"match": "none"})}, nil), "含健康信息（「失眠」）")
	reject("health in project name", changes(span("#3.0", "#3.2"), []M{exchange("#3.0", "#3.2", "家事", "回父母家", M{"match": "new", "name": "妈妈血压跟进"})}, nil), "项目名含健康信息")
	reject("multi-project", changes(span("#3.0", "#3.2"), []M{{"from": "#3.0", "to": "#3.2", "title": "黄总谈话", "summary": "OKR 与康宁", "projects": both}},
		M{"create": []M{create("x3.0", "mine", "周一前交 OKR", "#3.1")}}), "关联了多个项目，承诺须写 project")
	reject("unknown project", changes(span("#3.0", "#3.2"), []M{{"from": "#3.0", "to": "#3.2", "title": "a", "summary": "a", "projects": both}},
		M{"create": []M{{"exchange": "x3.0", "kind": "mine", "content": "x", "project": "家庭日常", "refs": []string{"#3.1"}}}}), "写 P-0001、P-0002、none 之一")
	reject("missing ref", changes(span("#3.0", "#3.2"), []M{exchange("#3.0", "#3.2", "a", "a", M{"match": "none"})}, M{"create": []M{create("x3.0", "mine", "x", "#3.9")}}), "#3.9 不存在")
	reject("outside source", changes(span("#3.0", "#3.1"), []M{exchange("#3.0", "#3.2", "a", "a", M{"match": "none"})}, nil), "不在本次读到的范围")

	out := invoke(x, "ses", "file_changes", changes(span("#3.0", "#3.2"), []M{{"from": "#3.0", "to": "#3.2", "title": "黄总谈话", "summary": "OKR 与康宁", "projects": both,
		"decisions": []any{M{"text": "OKR 下周一交", "project": "P-0001"}, M{"text": "源码不给", "project": "P-0002"}}}},
		M{"create": []M{{"exchange": "x3.0", "kind": "mine", "content": "交 OKR 初稿", "due_text": "下周一前", "project": "P-0001", "refs": []string{"#3.1"}},
			{"exchange": "x3.0", "kind": "mine", "content": "请黄总吃饭", "project": "none", "refs": []string{"#3.2"}}}}), cites...)
	contains(t, "accepted", out, "交谈 x3.0「黄总谈话」：归入 P-0001 OKR 汇报；归入 P-0002 康宁交付", "C-0001（已核实）用户答应：交 OKR 初稿，期限 2026-10-26（下周一前）")
	if d := invoke(x, "", "project_digest", M{"project": "P-0002"}); !strings.Contains(d, "源码不给") || strings.Contains(d, "OKR 下周一交") {
		t.Fatalf("project_digest = %q", d)
	}
	// 重交只写承诺：交谈随上次的变更集一起被取代，拒绝时说明重交是整体取代。
	out = invoke(x, "ses", "file_changes", changes(span("#3.0", "#3.2"), nil, M{"create": []M{create("x3.0", "mine", "x", "#3.1")}}), cites...)
	contains(t, "refile without exchanges", out, "没有被接受", "exchange x3.0 不在本变更集中：它是这个 source 上次交出的")
	// 承诺误嵌在 exchanges 里时照样记下。
	out = invoke(x, "ses", "file_changes", M{"source": span("#3.0", "#3.2"), "exchanges": M{
		"create":      []M{exchange("#3.0", "#3.2", "a", "a", M{"match": "none"})},
		"commitments": M{"create": []M{create("x3.0", "mine", "交 OKR 初稿", "#3.1")}},
	}}, cites...)
	contains(t, "nested commitments", out, "（已核实）用户答应：交 OKR 初稿")
}

// 归集的对账（xinli-project）：一个项目一天一个变更集，依据交谈摘要，受同样的 rev 检查。白天已依据原话改期的承诺，
// 按过时的 rev 再改被拒绝。只接受"整理今天"那次 Run 的 file_day。
func TestProjectReconcileAndFileDay(t *testing.T) {
	x := newServer(nil)
	x.Seed("bl", "u", SeedData{Projects: []Project{{ID: "P-0001", Name: "新一季采购"}},
		Commitments: []SeedCommitment{{ID: "C-0001", Kind: "mine", Content: "发报价单", Project: "P-0001"}, {ID: "C-0002", Kind: "theirs", Content: "寄框架合同", Project: "P-0001", DueDate: "2026-10-26"}},
		Days:        map[string]string{"2026-10-24": "王总来电：报价单收到了，合同推迟到 11 月 2 日寄出。"}})
	x.Bind("bl", "u", "P-0001", "ses_p")
	sum := invoke(x, "ses_p", "day_summary", M{"date": "2026-10-24"})
	contains(t, "day_summary", sum, "交谈 x2026-10-24", "C-0001（rev 1）用户答应：发报价单")
	if out := invoke(x, "ses_p", "update_commitments", M{"date": "2026-10-24", "close": []M{{"id": "C-0001", "rev": 1, "status": "done"}}}); !strings.Contains(out, "须用 exchanges 写明依据的交谈 ID") {
		t.Fatalf("no basis = %q", out)
	}
	reconcile := M{"date": "2026-10-24", "close": []M{{"id": "C-0001", "rev": 1, "status": "done", "exchanges": []string{"x2026-10-24"}}},
		"revise": []M{{"id": "C-0002", "rev": 1, "due_date": "2026-11-02", "exchanges": []string{"x2026-10-24"}}}}
	out := invoke(x, "ses_p", "update_commitments", reconcile)
	contains(t, "update_commitments", out, "关闭 C-0001（done）", "修订 C-0002，现在 rev 2")
	if c := x.State("bl", "u").Commitments[1]; c.DueDate != "2026-11-02" || c.Revisions[1].Basis() != "摘要" {
		t.Fatalf("C-0002 = %+v", c)
	}
	// 重交同一天：整体取代，不叠加两次。照抄现在的期限不算改动。
	invoke(x, "ses_p", "update_commitments", reconcile)
	if out := invoke(x, "ses_p", "update_commitments", M{"date": "2026-10-26", "revise": []M{{"id": "C-0002", "rev": 2, "due_date": "2026-11-02", "exchanges": []string{"x2026-10-24"}}}}); !strings.Contains(out, "C-0002：与现在相同，没有改动") {
		t.Fatalf("no-op revise = %q", out)
	}
	if c := x.State("bl", "u").Commitments[1]; c.Rev() != 2 {
		t.Fatalf("reconciled twice: %+v", c.Revisions)
	}
	if out := invoke(x, "ses_p", "update_commitments", M{"date": "2026-10-25", "revise": []M{{"id": "C-0002", "rev": 1, "due_date": "2026-11-05", "exchanges": []string{"x2026-10-24"}}}}); !strings.Contains(out, "rev 是 2 而不是 1") {
		t.Fatalf("stale project rev = %q", out)
	}

	x.Submitted("bl", "u", "ses_day", "整理交谈")
	if out := invoke(x, "ses_day", "file_day", M{"day": M{"headline": "x"}}); !strings.Contains(out, "没有收下") || len(x.State("bl", "u").Days) != 0 {
		t.Fatalf("daytime file_day = %q", out)
	}
	x.Submitted("bl", "u", "ses_day", "整理今天")
	if out := invoke(x, "ses_day", "file_day", M{"day": M{"headline": "x"}, "awareness": M{"suggestions": []string{"连续失眠，注意休息", "妈妈血压高"}}}); !strings.Contains(out, "「失眠」「血压」") {
		t.Fatalf("file_day with health = %q", out)
	}
	if out := invoke(x, "ses_day", "file_day", M{"day": M{"headline": "x"}}); !strings.Contains(out, "已收到") {
		t.Fatalf("file_day = %q", out)
	}
}

// 每周回顾：归档 30 天没有新交谈、也没有未完成承诺的项目；合并提议由用户确认后交谈与承诺归到保留的项目；file_week 记下
// 本周觉察；Week 为本周创建一次回顾的 Session，并向本周有交谈的项目提交"重写现状"。
func TestWeeklyReview(t *testing.T) {
	biz := &fakeBusiness{inputs: map[string][]string{}, texts: map[string]string{}, done: make(chan struct{}, 8)}
	now := time.Date(2026, 10, 25, 22, 0, 0, 0, time.Local)
	x := &Server{ctx: context.Background(), users: map[string]*user{}, Business: biz, Now: func() time.Time { return now }}
	x.Seed("bl", "u", SeedData{
		Projects: []Project{{ID: "P-0001", Name: "新办公室装修", Status: "地板定了"}, {ID: "P-0002", Name: "办公室搬迁", Status: "十二月搬"},
			{ID: "P-0003", Name: "旧事", Status: "早就结束了"}, {ID: "P-0004", Name: "旧事但有承诺", Status: "还欠一件事"}},
		Exchanges: []SeedExchange{{Date: "2026-10-24", Title: "看样板", Project: "P-0001"}, {Date: "2026-10-23", Title: "定搬家公司", Project: "P-0002"},
			{Date: "2026-08-01", Title: "收尾", Project: "P-0003"}, {Date: "2026-08-01", Title: "收尾", Project: "P-0004"}},
		Commitments: []SeedCommitment{{Kind: "mine", Content: "寄发票", Project: "P-0004"}, {Kind: "theirs", Content: "报价", Project: "P-0002"}},
		Filed: []Day{{Date: "2026-10-24", Headline: "看样板", Awareness: Awareness{Habits: []string{"答应时给出期限"}}},
			{Date: "2026-10-10", Headline: "上上周", Awareness: Awareness{Habits: []string{"太早了"}}}},
	})
	x.Bind("bl", "u", "P-0001", "ses_p1")
	if got := x.Archive("bl", "u"); len(got) != 1 || got[0] != "P-0003" {
		t.Fatalf("archived %v", got)
	}
	if out := invoke(x, "", "find_related", M{"query": "旧事"}); strings.Contains(out, "P-0003") || !strings.Contains(out, "P-0004") {
		t.Fatalf("find_related after archive = %q", out)
	}
	if out := invoke(x, "ses_w", "week_overview", M{}); !strings.Contains(out, "2026-10-24：看样板") || strings.Contains(out, "上上周") ||
		!strings.Contains(out, "沟通习惯：答应时给出期限") {
		t.Fatalf("week_overview = %q", out)
	}
	if out := invoke(x, "ses_w", "similar_projects", M{}); !strings.Contains(out, "第 1 对") || strings.Contains(out, "P-0003") {
		t.Fatalf("similar_projects = %q", out)
	}
	if out := invoke(x, "ses_w", "propose_merge", M{"into": "P-0001", "from": "P-0002", "reason": "同一次搬迁"}); !strings.Contains(out, "M-0001") {
		t.Fatalf("propose_merge = %q", out)
	}
	if out := invoke(x, "ses_w", "propose_merge", M{"into": "P-0002", "from": "P-0001", "reason": "重试"}); !strings.Contains(out, "已经提议过") {
		t.Fatalf("repeated propose_merge = %q", out)
	}
	if err := x.ResolveMerge("bl", "u", "M-0001", true); err != nil {
		t.Fatal(err)
	}
	st := x.State("bl", "u")
	if st.Projects[1].MergedInto != "P-0001" || st.Exchanges[1].InProjects()[0] != "P-0001" || st.Commitments[1].Project != "P-0001" || st.Merges[0].Status != "accepted" {
		t.Fatalf("after merge: %+v", st)
	}
	if out := invoke(x, "ses_w", "file_week", M{"habits": []M{{"habit": "给出期限", "days": []string{"2026-10-21", "2026-10-24"}},
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
