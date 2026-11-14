package eval

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"yanshi/internal/agentdef"
	"yanshi/internal/eval/xinlifixture"
	"yanshi/internal/eventlog"
	"yanshi/internal/model"
	"yanshi/internal/service"
)

func TestCaptureBusinessDay(t *testing.T) {
	for _, tc := range []struct {
		name string
		now  time.Time
		want string
	}{
		{"before local midnight", time.Date(2026, 10, 23, 15, 59, 0, 0, time.UTC), "2026-10-23"},
		{"after local midnight", time.Date(2026, 10, 23, 16, 1, 0, 0, time.UTC), "2026-10-24"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := captureBusinessDay(tc.now); got != tc.want {
				t.Fatalf("business day = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestCaptureSubmissionPathsAndReferences(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agents, err := agentdef.NewRegistry(
		&agentdef.Def{Name: "xinli-day", Version: "2", Model: "script/any", Capabilities: []string{"mcp:xinli/captures", "mcp:xinli/file_changes"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	gw := model.NewGateway()
	gw.Register("script", script{})
	c := &Case{Name: "submission", Agent: "xinli-day"}
	cfg := Config{Agents: agents, Model: gw, Parallel: 1}
	cfg.defaults()
	in, err := start(ctx, cfg, []*Case{c})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(in.close)
	// 没有 setup.xinli 时仍由 AgentDef 的白名单启动夹具。
	if in.xinli == nil {
		t.Fatal("MCP capture agent did not start fixture")
	}
	sid, err := in.svc.Create(ctx, service.CreateRequest{BusinessLine: BusinessLine, EndUser: "submission", Agent: "xinli-day", AgentVersion: in.version[c.Name]})
	if err != nil {
		t.Fatal(err)
	}
	day := captureBusinessDay(time.Now())
	if err := in.xinli.BindDay(ctx, BusinessLine, "submission", day, sid); err != nil {
		t.Fatal(err)
	}
	segs := []CaptureSegment{
		{Speaker: "other", Key: "same", Text: "先确认。", At: "2026-10-23 23:59"},
		{Speaker: "wearer", Text: "我来办。", At: "2026-10-24 00:01"},
	}
	refs, err := in.submitCaptures(ctx, sid, day, 1, segs)
	if err != nil || !reflect.DeepEqual(refs, []string{"1.0", "2.0"}) {
		t.Fatalf("Persona references = %v, %v", refs, err)
	}
	retry, err := in.submitCaptures(ctx, sid, day, 1, segs)
	if err != nil || !reflect.DeepEqual(retry, refs) {
		t.Fatalf("retry references = %v, %v", retry, err)
	}
	legacyBatches := [][]xinlifixture.CaptureSegment{
		{{Speaker: "other", SpeakerKey: "other-1", Text: "先确认。", SpokenAt: "2026-10-23T15:59:00Z", TimeZone: "Asia/Shanghai"}},
		{{Speaker: "wearer", Text: "我来办。", SpokenAt: "2026-10-23T16:01:00Z", TimeZone: "Asia/Shanghai"}},
	}
	for i, batch := range legacyBatches {
		got, err := in.xinli.AppendCaptures(ctx, BusinessLine, "submission", day, fmt.Sprintf("cap_%s_1_%d", sid, i), batch)
		if err != nil || !reflect.DeepEqual(got, refs[i:i+1]) {
			t.Fatalf("legacy wire batch %d = %v, %v", i, got, err)
		}
	}
	events, err := eventlog.ReadAll(ctx, in.svc.Store.Log, sid, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.GetCaptureAppended() != nil {
			t.Fatal("Persona capture submission wrote raw captures to kernel")
		}
	}
	from, to, err := in.nextSpan(ctx, sid, true)
	if err != nil || from != refs[0] || to != refs[1] {
		t.Fatalf("Persona span = %q..%q, %v", from, to, err)
	}
	topics := map[string]string{sid + "/" + refs[0]: "one", sid + "/" + refs[1]: "two", "other/1.0": "wrong"}
	got := rangeTopics(&xinlifixture.Exchange{Session: sid, From: "#" + refs[0], To: "#" + refs[1]}, topics)
	if !reflect.DeepEqual(got, []string{"one", "two"}) {
		t.Fatalf("topic mapping = %v", got)
	}
	// 后一轮跨午夜也不重新绑定，引用继续在最初的业务日递增。
	later, err := in.submitCaptures(ctx, sid, day, 2, []CaptureSegment{{Speaker: "wearer", Text: "补充一句。", At: "2026-10-24 00:02"}})
	if err != nil || !reflect.DeepEqual(later, []string{"3.0"}) {
		t.Fatalf("later turn references = %v, %v", later, err)
	}
	// 同日的新 Session 只换绑定，不重置原文、批次编号或处理进度。
	position, err := in.xinli.CapturePosition(ctx, BusinessLine, "submission", sid)
	if err != nil {
		t.Fatal(err)
	}
	nextSID, err := in.svc.Create(ctx, service.CreateRequest{BusinessLine: BusinessLine, EndUser: "submission", Agent: "xinli-day", AgentVersion: in.version[c.Name]})
	if err != nil {
		t.Fatal(err)
	}
	if err := in.xinli.BindDay(ctx, BusinessLine, "submission", day, nextSID); err != nil {
		t.Fatal(err)
	}
	if got, err := in.xinli.CapturePosition(ctx, BusinessLine, "submission", nextSID); err != nil || got != position {
		t.Fatalf("rebound position = %q, %v; want %q", got, err, position)
	}
	if _, err := in.xinli.CapturePosition(ctx, BusinessLine, "submission", sid); err == nil {
		t.Fatal("old Session remained bound")
	}
	from, to, err = in.nextSpan(ctx, nextSID, true)
	if err != nil || from != refs[0] || to != later[0] {
		t.Fatalf("rebound span = %q..%q, %v", from, to, err)
	}
	newRefs, err := in.submitCaptures(ctx, nextSID, day, 3, []CaptureSegment{{Speaker: "wearer", Text: "第二个 Session 的旁听。", At: "2026-10-25 09:00"}})
	if err != nil || !reflect.DeepEqual(newRefs, []string{"4.0"}) {
		t.Fatalf("rebound submission references = %v, %v", newRefs, err)
	}
	inheritCaptureMarks(topics, nextSID, day, map[string]string{sid: day})
	got = rangeTopics(&xinlifixture.Exchange{Session: nextSID, From: "#" + refs[0], To: "#" + refs[1]}, topics)
	if !reflect.DeepEqual(got, []string{"one", "two"}) {
		t.Fatalf("rebound topic mapping = %v", got)
	}
	// 编排预览已经走完第一时段，但只有模型的真实 MCP 读页才能授权推进处理进度。
	from, to, err = in.nextSpan(ctx, nextSID, true)
	if err != nil || from != "1.0" || to != "3.0" {
		t.Fatalf("preview before filing = %q..%q, %v", from, to, err)
	}
	file := fmt.Sprintf(`call mcp_xinli__file_changes {"source":{"from":"#%s","to":"#%s"},"exchanges":{},"commitments":{}}`, from, to)
	run := func(input string) {
		t.Helper()
		obs, err := in.turn(ctx, nextSID, Turn{Input: input}, 10*time.Second)
		if err != nil || obs == nil || obs.status != "completed" {
			t.Fatalf("capture read/file turn: observation=%+v, err=%v", obs, err)
		}
	}
	run(file)
	if got, err := in.xinli.CapturePosition(ctx, BusinessLine, "submission", nextSID); err != nil || got != "0" {
		t.Fatalf("preview authorized processing: position=%q, err=%v", got, err)
	}
	run(`call mcp_xinli__captures {"after":"new","all":true}`)
	run(file)
	if got, err := in.xinli.CapturePosition(ctx, BusinessLine, "submission", nextSID); err != nil || got != to {
		t.Fatalf("actual read did not authorize processing: position=%q, err=%v; want %q", got, err, to)
	}
	// 同 ID 用手写的线上值重交，确保 runner 没有丢字段、回填缺省值或过滤隐藏片段。
	metadata := []CaptureSegment{
		{Speaker: "wearer", SpeakerGrade: "timeline", Text: "明天🙂发报价。", At: "2026-10-25 10:00", LowSupportSpans: []CaptureLowSupportSpan{{Start: 0, End: 2, CriticalKinds: []string{"date"}}, {Start: 2, End: 3, CriticalKinds: []string{"none"}}}},
		{Speaker: "other", Key: "wang", SpeakerRef: "P-0012", Text: "背景中的原话", At: "2026-10-25 10:00", Disposition: "quarantine", DispositionReason: "playback", Background: true, LowSupportSpans: []CaptureLowSupportSpan{{Start: 0, End: 2, CriticalKinds: []string{"person", "number"}}}},
		{Speaker: "unknown", Text: "身份不明", At: "2026-10-25 10:00"},
		{Speaker: "wearer", SpeakerGrade: "evidence", Text: "明确的原话", At: "2026-10-25 10:00", Disposition: "accept"},
	}
	metadataRefs, err := in.submitCaptures(ctx, nextSID, day, 4, metadata)
	if err != nil || !reflect.DeepEqual(metadataRefs, []string{"5.0", "5.1", "5.2", "5.3"}) {
		t.Fatalf("metadata references = %v, %v", metadataRefs, err)
	}
	wire := []xinlifixture.CaptureSegment{
		{Speaker: "wearer", SpeakerGrade: "timeline", Text: "明天🙂发报价。", SpokenAt: "2026-10-25T02:00:00Z", TimeZone: "Asia/Shanghai", LowSupportSpans: []xinlifixture.LowSupportSpan{{Start: 0, End: 2, CriticalKinds: []string{"date"}}, {Start: 2, End: 3, CriticalKinds: []string{"none"}}}},
		{Speaker: "other", SpeakerKey: "other-1", SpeakerRef: "P-0012", Text: "背景中的原话", SpokenAt: "2026-10-25T02:00:00Z", TimeZone: "Asia/Shanghai", Disposition: "quarantine", DispositionReason: "playback", Background: true, LowSupportSpans: []xinlifixture.LowSupportSpan{{Start: 0, End: 2, CriticalKinds: []string{"person", "number"}}}},
		{Speaker: "unknown", Text: "身份不明", SpokenAt: "2026-10-25T02:00:00Z", TimeZone: "Asia/Shanghai"},
		{Speaker: "wearer", SpeakerGrade: "evidence", Text: "明确的原话", SpokenAt: "2026-10-25T02:00:00Z", TimeZone: "Asia/Shanghai", Disposition: "accept"},
	}
	captureID := fmt.Sprintf("cap_%s_4_0", nextSID)
	if got, err := in.xinli.AppendCaptures(ctx, BusinessLine, "submission", day, captureID, wire); err != nil || !reflect.DeepEqual(got, metadataRefs) {
		t.Fatalf("metadata round trip = %v, %v", got, err)
	}
	for _, change := range []struct {
		name   string
		mutate func([]xinlifixture.CaptureSegment)
	}{
		{"speaker", func(gs []xinlifixture.CaptureSegment) { gs[2].Speaker = "wearer" }},
		{"grade", func(gs []xinlifixture.CaptureSegment) { gs[0].SpeakerGrade = "evidence" }},
		{"disposition", func(gs []xinlifixture.CaptureSegment) { gs[3].Disposition = "" }},
		{"reason", func(gs []xinlifixture.CaptureSegment) { gs[1].DispositionReason = "suspected_replay" }},
		{"background", func(gs []xinlifixture.CaptureSegment) { gs[1].Background = false }},
		{"start", func(gs []xinlifixture.CaptureSegment) { gs[1].LowSupportSpans[0].Start = 1 }},
		{"end", func(gs []xinlifixture.CaptureSegment) { gs[1].LowSupportSpans[0].End = 3 }},
		{"kinds", func(gs []xinlifixture.CaptureSegment) { gs[1].LowSupportSpans[0].CriticalKinds[0] = "date" }},
		{"spans", func(gs []xinlifixture.CaptureSegment) { gs[1].LowSupportSpans = nil }},
	} {
		t.Run("metadata identity/"+change.name, func(t *testing.T) {
			changed := slices.Clone(wire)
			for i := range changed {
				changed[i].LowSupportSpans = slices.Clone(changed[i].LowSupportSpans)
				for j := range changed[i].LowSupportSpans {
					changed[i].LowSupportSpans[j].CriticalKinds = slices.Clone(changed[i].LowSupportSpans[j].CriticalKinds)
				}
			}
			change.mutate(changed)
			if _, err := in.xinli.AppendCaptures(ctx, BusinessLine, "submission", day, captureID, changed); err == nil {
				t.Fatal("Persona accepted changed metadata under the same capture_id")
			}
		})
	}
	if got, err := in.submitCaptures(ctx, nextSID, day, 4, metadata); err != nil || !reflect.DeepEqual(got, metadataRefs) {
		t.Fatalf("metadata retry = %v, %v", got, err)
	}
}

func TestCaptureMarksFollowSameDayRebinding(t *testing.T) {
	marks := map[string]string{"first/1.0": "old", "first/2.0": "two", "other/1.0": "wrong"}
	days := map[string]string{"first": "2026-10-28", "other": "2026-10-27"}
	inheritCaptureMarks(marks, "second", "2026-10-28", days)
	days["second"] = "2026-10-28"
	marks["second/3.0"] = "new"
	inheritCaptureMarks(marks, "third", "2026-10-28", days)
	want := map[string]string{"first/1.0": "old", "first/2.0": "two", "other/1.0": "wrong",
		"second/1.0": "old", "second/2.0": "two", "second/3.0": "new",
		"third/1.0": "old", "third/2.0": "two", "third/3.0": "new"}
	if !reflect.DeepEqual(marks, want) {
		t.Fatalf("rebound marks = %v", marks)
	}
}

func TestGoldLabelsFollowSameDayRebinding(t *testing.T) {
	book := newGoldBook(testGold())
	labels := testHear(book)
	inheritCaptureMarks(labels, "next", "2026-10-28", map[string]string{"s": "2026-10-28"})
	created := commitment("C1", "mine", "2026-10-28", "open", "#1.0")
	created.Session = "next"
	revised := commitment("C2", "mine", "2026-10-27", "open", "#9.9")
	revised.Revisions = []xinlifixture.Revision{{Op: "revise", Changeset: "new-cs", Refs: []string{"#2.0"}}}
	state := &xinlifixture.State{Commitments: []xinlifixture.Commitment{created, revised}, Changesets: []xinlifixture.Changeset{{ID: "new-cs", Session: "next"}}}
	matched := book.match(state, labels)
	for _, id := range []string{"C1", "C2"} {
		if got := matched[id]; got == nil || got.ID != "accept" {
			t.Fatalf("rebound gold %s = %v; want accept", id, got)
		}
	}
}
