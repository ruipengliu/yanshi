package storetest

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"testing"
	"time"

	"persona/domain"
	"persona/store"
)

// 按需读取的差分测试（Persona ADR-0002）：同一 owner 上，按读取计划装载的结果必须与整体装载逐项相同。
// 账本覆盖来源重交、修订、关闭、交谈与承诺的合并、擦除后换绑、项目 Session 的跨日对账、用户修改、项目合并
// 与含 U+0000 的预置承诺。

var (
	partialNow      = time.Date(2026, 10, 14, 20, 0, 0, 0, time.UTC)
	partialID       = regexp.MustCompile(`- 承诺 (C-[0-9]+)（`)
	partialProjects = 5
)

const partialDays = 14

type partialObject = map[string]any

func partialDay(day int) time.Time { return partialNow.AddDate(0, 0, day-partialDays+1) }

func partialSession(day int) string { return "day-" + partialDay(day).Format(time.DateOnly) }

func partialBatch(day, number int) []domain.CaptureSegment {
	at := partialDay(day).Truncate(24 * time.Hour).Add(8*time.Hour + time.Duration(number-1)*24*time.Second)
	out := make([]domain.CaptureSegment, 3)
	for i := range out {
		out[i] = domain.CaptureSegment{Speaker: "wearer", Text: fmt.Sprintf("第%d天第%d批第%d句：确认材料与报价", day, number, i), SpokenAt: at.Add(time.Duration(i) * 6 * time.Second).Format(time.RFC3339), TimeZone: "UTC"}
		if i == 1 {
			out[i].Speaker, out[i].SpeakerKey = "other", "colleague"
		}
	}
	return out
}

func partialCitations(day, batches int) map[string]domain.Citation {
	out := map[string]domain.Citation{}
	for number := 1; number <= batches; number++ {
		for i, segment := range partialBatch(day, number) {
			ref := fmt.Sprintf("#%d.%d", number, i)
			at, _ := time.Parse(time.RFC3339, segment.SpokenAt)
			out[ref] = domain.Citation{Ref: ref, Speaker: segment.Speaker, SpeakerKey: segment.SpeakerKey, SpokenAt: at, TimeZone: "UTC"}
		}
	}
	return out
}

func partialInvoke(book *domain.Book, session string, args any, cites map[string]domain.Citation, now time.Time, method func(*domain.Call) string, prefix string) (string, error) {
	data, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	call := domain.NewCall(book, session, data, cites, now)
	out := method(call)
	if err := call.Err(); err != nil {
		return "", err
	}
	if !strings.HasPrefix(out, prefix) {
		return "", fmt.Errorf("receipt did not start with %q: %s", prefix, out)
	}
	return out, nil
}

// partialFixture 只经领域命令构造账本，与评测和成本测试的写入路径相同。
func partialFixture(book *domain.Book) (sessions []string, err error) {
	rng := rand.New(rand.NewSource(20261114))
	book.Seed(domain.SeedData{Commitments: []domain.SeedCommitment{
		{ID: "C-9000", Kind: "mine", Content: "预置\u0000承诺", Counterpart: "同事00", DueDate: "2026-10-20"},
		{ID: "C-9001", Kind: "theirs", Content: "对方交付报价", Counterpart: "同事01"},
	}}, partialDay(0))
	const batches = 20
	for day := range partialDays {
		now, date, session := partialDay(day), partialDay(day).Format(time.DateOnly), partialSession(day)
		book.SetCaptureClock(func() time.Time { return now })
		if err := book.BindDay(date, session); err != nil {
			return nil, err
		}
		for number := 1; number <= batches; number++ {
			if _, err := book.AppendCaptures(date, fmt.Sprintf("batch-%04d", number), partialBatch(day, number)); err != nil {
				return nil, err
			}
		}
		sessions = append(sessions, session)
		book.Submitted(session, "整理今天")
		exchanges, commitments := []partialObject{}, []partialObject{}
		for i := range 5 {
			project := rng.Intn(partialProjects)
			link := partialObject{"match": "existing", "id": fmt.Sprintf("P-%04d", 101+project)}
			switch {
			case day == 0:
				link = partialObject{"match": "new", "name": fmt.Sprintf("交付项目%02d", i)}
			case i == 4:
				link = partialObject{"match": "unsure", "id": fmt.Sprintf("P-%04d", 101+project)}
			}
			exchanges = append(exchanges, partialObject{"from": fmt.Sprintf("#%d.0", i+1), "to": fmt.Sprintf("#%d.2", i+1), "title": fmt.Sprintf("交付讨论%02d", i), "summary": "核对交付材料和报价。", "projects": []partialObject{link}})
			if i < 4 {
				kind, index := "mine", 0
				if i == 2 {
					kind, index = "theirs", 1
				}
				commitments = append(commitments, partialObject{"exchange": fmt.Sprintf("x%d.0", i+1), "kind": kind, "content": fmt.Sprintf("确认交付材料%02d", i), "counterpart": fmt.Sprintf("同事%02d", rng.Intn(3)), "due_date": now.AddDate(0, 0, 2+i).Format(time.DateOnly), "refs": []string{fmt.Sprintf("#%d.%d", i+1, index)}})
			}
		}
		file := func(args any) (string, error) {
			return partialInvoke(book, session, args, partialCitations(day, batches), now, (*domain.Call).FileChanges, "已收下变更集")
		}
		args := partialObject{"source": partialObject{"from": "#1.0", "to": "#5.2"}, "exchanges": partialObject{"create": exchanges}, "commitments": partialObject{"create": commitments}}
		receipt, err := file(args)
		if err != nil {
			return nil, err
		}
		ids := partialID.FindAllStringSubmatch(receipt, -1)
		if len(ids) != 4 {
			return nil, fmt.Errorf("day %d: expected 4 commitment IDs: %s", day, receipt)
		}
		if day%3 == 0 {
			// 来源重交整体取代上一次，承诺沿用 ID。
			for i := range commitments {
				commitments[i]["id"] = ids[i][1]
			}
			exchanges[0]["summary"] = "重新核对交付材料和报价。"
			if _, err := file(args); err != nil {
				return nil, err
			}
		}
		if _, err := file(partialObject{"source": partialObject{"from": "#6.0", "to": "#6.0"}, "commitments": partialObject{"revise": []partialObject{{"id": ids[0][1], "rev": 1, "content": "确认交付材料第二轮", "refs": []string{"#6.0"}}}}}); err != nil {
			return nil, err
		}
		if day%2 == 1 {
			if _, err := file(partialObject{"source": partialObject{"from": "#7.0", "to": "#7.2"}, "commitments": partialObject{"close": []partialObject{{"id": ids[1][1], "rev": 1, "status": "done", "refs": []string{"#7.0"}}, {"id": ids[2][1], "rev": 1, "status": "cancelled", "refs": []string{"#7.1"}}}}}); err != nil {
				return nil, err
			}
		}
		if day%4 == 2 {
			if _, err := file(partialObject{"source": partialObject{"day": true}, "exchanges": partialObject{"merge": []partialObject{{"into": "x4.0", "from": []string{"x5.0"}}}}, "commitments": partialObject{"merge": []partialObject{{"into": ids[0][1], "from": []string{ids[3][1]}, "reason": "同一次确认"}}}}); err != nil {
				return nil, err
			}
		}
		if day%5 == 4 {
			// 用户修改是作者为 user、没有 Session 的变更集。
			if err := book.Edit(ids[0][1], "", now.AddDate(0, 0, 9).Format(time.DateOnly), now); err != nil {
				return nil, err
			}
		}
		if _, err := partialInvoke(book, session, partialObject{"day": partialObject{"headline": fmt.Sprintf("第%d天的交付讨论", day), "highlights": []string{"明确下一次核对时间"}}}, nil, now, (*domain.Call).FileDay, "已收到"); err != nil {
			return nil, err
		}
		if day == 5 {
			// 擦除后换绑：同一业务日有两个 Session，之后的整理落在新 Session 上。
			task, err := book.Erase(date, "20.0", "20.0", now)
			if err != nil {
				return nil, err
			}
			if err := book.CompleteDeletionTask(task.ID, nil, now); err != nil {
				return nil, err
			}
			session += "-rebuilt"
			if err := book.BindDay(date, session); err != nil {
				return nil, err
			}
			sessions = append(sessions, session)
			if _, err := partialInvoke(book, session, partialObject{"source": partialObject{"from": "#8.0", "to": "#8.0"}, "commitments": partialObject{"create": []partialObject{{"exchange": "x1.0", "kind": "mine", "content": "换绑后新建的承诺", "counterpart": "同事01", "refs": []string{"#8.0"}}}}}, partialCitations(day, batches), now, (*domain.Call).FileChanges, "已收下变更集"); err != nil {
				return nil, err
			}
		}
	}
	for project := range partialProjects {
		id, session := fmt.Sprintf("P-%04d", 101+project), fmt.Sprintf("project-%02d", project)
		book.Bind(id, session)
		sessions = append(sessions, session)
	}
	// 项目 Session 的对账跨日修订承诺：变更集的 Session 是项目 Session。
	open, err := book.QueryCommitments(domain.CommitmentFilter{})
	if err != nil {
		return nil, err
	}
	for _, m := range open {
		if m.Project != "" {
			p := strings.TrimPrefix(m.Project, "P-0")
			var n int
			fmt.Sscanf(p, "%d", &n)
			session := fmt.Sprintf("project-%02d", n-101)
			args := partialObject{"date": partialDay(partialDays - 1).Format(time.DateOnly), "revise": []partialObject{{"id": m.ID, "rev": m.Rev(), "due_date": partialNow.AddDate(0, 0, 5).Format(time.DateOnly), "exchanges": []string{"x1.0"}}}}
			if _, err := partialInvoke(book, session, args, nil, partialNow, (*domain.Call).UpdateCommitments, "已收下"); err != nil {
				return nil, err
			}
			break
		}
	}
	week := "week-00"
	book.BindWeek(partialDay(0).Format(time.DateOnly), week)
	if _, err := partialInvoke(book, week, partialObject{"into": "P-0101", "from": "P-0102", "reason": "同一个客户"}, nil, partialNow, (*domain.Call).ProposeMerge, "已提议"); err != nil {
		return nil, err
	}
	if err := book.ResolveMerge("M-0001", true); err != nil {
		return nil, err
	}
	book.SetCaptureClock(func() time.Time { return partialNow })
	return append(sessions, "unknown-session", ""), nil
}

type partialRead struct {
	name string
	plan domain.ReadPlan
	run  func(*domain.Book) (any, error)
}

func partialReads(sessions, ids, dates []string) []partialRead {
	var reads []partialRead
	tool := func(name, session string, args any, method func(*domain.Call) string) {
		data, _ := json.Marshal(args)
		reads = append(reads, partialRead{
			name: fmt.Sprintf("%s %s %s", name, session, data),
			plan: domain.ToolReadPlan(name, session, data),
			run: func(b *domain.Book) (any, error) {
				call := domain.NewCall(b, session, data, nil, partialNow)
				out := method(call)
				return out, call.Err()
			},
		})
	}
	for _, session := range sessions {
		tool("day_ledger", session, partialObject{}, (*domain.Call).DayLedger)
	}
	filters := []partialObject{{}, {"status": "all"}, {"status": "done"}, {"status": "cancelled"}, {"status": "bogus"}, {"kind": "theirs"},
		{"project": "P-0102"}, {"project": "P-0101", "status": "all"}, {"counterpart": "同事01"}, {"query": "交付材料"},
		{"due_from": "2026-10-10", "due_to": "2026-10-20"}, {"kind": "nobody"}}
	for _, f := range filters {
		tool("list_commitments", "day", f, (*domain.Call).ListCommitments)
	}
	tool("list_commitments", "day", partialObject{"status": "all", "after": ids[len(ids)/2]}, (*domain.Call).ListCommitments)
	for _, q := range []partialObject{{"query": "交付材料"}, {"query": "报价", "counterparts": []string{"同事00"}}, {"query": ""}} {
		tool("find_related", "day", q, (*domain.Call).FindRelated)
	}
	query := func(name, id, date string, filter domain.CommitmentFilter, run func(*domain.Book) (any, error)) {
		reads = append(reads, partialRead{name: fmt.Sprintf("query %s %s %s %+v", name, id, date, filter), plan: domain.QueryReadPlan(name, id, date, filter), run: run})
	}
	for _, f := range []domain.CommitmentFilter{{}, {Status: "all"}, {Status: "done", Kind: "mine"}, {Project: "P-0102", Status: "all"}, {Status: "bogus"}} {
		query("commitments", "", "", f, func(b *domain.Book) (any, error) { return b.QueryCommitments(f) })
	}
	for _, id := range append(ids, "C-0000") {
		query("history", id, "", domain.CommitmentFilter{}, func(b *domain.Book) (any, error) { return b.QueryCommitment(id) })
	}
	for _, date := range append(dates, "2020-01-01") {
		query("day", "", date, domain.CommitmentFilter{}, func(b *domain.Book) (any, error) { return b.QueryDay(date) })
	}
	return reads
}

func comparable(value any, err error) string {
	data, merr := json.Marshal(value)
	if merr != nil {
		return "marshal: " + merr.Error()
	}
	if err != nil {
		return "error: " + err.Error() + " " + string(data)
	}
	return string(data)
}

func partialReadsMatch(t *testing.T, factory func() store.Store) {
	PartialReads(t, factory(), false)
}

// PartialReads 构造固定账本，逐项比较按需读取与整体装载；requirePartial 为真时还要求计划确实以部分账本执行。
func PartialReads(t *testing.T, s store.Store, requirePartial bool) {
	t.Helper()
	ctx := context.Background()
	var sessions, ids, dates []string
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		var err error
		sessions, err = partialFixture(b)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if stale, err := s.StaleOwners(ctx); err != nil || len(stale) != 0 {
		t.Fatalf("committed owner is stale: %v %v", stale, err)
	}
	if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
		state := b.State()
		for _, m := range state.Commitments {
			ids = append(ids, m.ID)
		}
		for _, d := range state.Days {
			dates = append(dates, d.Date)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	partials := 0
	for _, read := range partialReads(sessions, ids, dates) {
		var full, planned string
		if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
			full = comparable(read.run(b))
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.ViewPlanned(ctx, "bl", "u", read.plan, func(b *domain.Book) error {
			if b.Partial() {
				partials++
			}
			planned = comparable(read.run(b))
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if planned != full {
			t.Fatalf("%s: planned read differs from full load\nplanned: %s\nfull:    %s", read.name, planned, full)
		}
	}
	if requirePartial && partials == 0 {
		t.Fatal("no read used a partial book")
	}
}
