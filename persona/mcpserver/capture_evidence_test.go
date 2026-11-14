package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"persona/domain"
	"persona/pg/pgtest"
	"persona/store"
	"persona/store/pgstore"
)

type evidenceCase struct {
	name       string
	segment    domain.CaptureSegment
	kind       string
	unreliable bool
}

func captureEvidenceCases() []evidenceCase {
	wearer := domain.CaptureSegment{Speaker: "wearer", Text: "甲🙂明天交稿"}
	other := domain.CaptureSegment{Speaker: "other", SpeakerKey: "other-1", Text: wearer.Text}
	unknown := domain.CaptureSegment{Speaker: "unknown", Text: wearer.Text}
	evidence, timeline := wearer.Clone(), wearer.Clone()
	evidence.SpeakerGrade, timeline.SpeakerGrade = "evidence", "timeline"
	cases := []evidenceCase{
		{"legacy_wearer", wearer, "mine", false},
		{"evidence", evidence, "mine", false},
		{"timeline", timeline, "", false},
		{"legacy_other", other, "theirs", false},
		{"unknown", unknown, "", false},
	}
	for _, tc := range append([]evidenceCase(nil), cases...) {
		accepted := tc.segment.Clone()
		accepted.Disposition = "accept"
		cases = append(cases, evidenceCase{tc.name + "_accept", accepted, tc.kind, false})
		hidden := tc.segment.Clone()
		hidden.Disposition, hidden.DispositionReason = "quarantine", "playback"
		cases = append(cases, evidenceCase{tc.name + "_quarantine", hidden, "", false})
		if hidden.Speaker != "wearer" {
			background := tc.segment.Clone()
			background.Background = true
			cases = append(cases, evidenceCase{tc.name + "_background", background, "", false})
		}
	}
	for _, tc := range []evidenceCase{{"wearer", wearer, "mine", false}, {"other", other, "theirs", false}} {
		for _, kind := range []string{"person", "date", "number", "negation", "modality", "correction", "unknown", "none", ""} {
			g := tc.segment.Clone()
			span := domain.LowSupportSpan{Start: 1, End: 4}
			if kind != "" {
				span.CriticalKinds = []string{kind}
			}
			g.LowSupportSpans = []domain.LowSupportSpan{span}
			cases = append(cases, evidenceCase{tc.name + "_low_support_" + kind, g, tc.kind, kind != "none"})
		}
	}
	// 隐藏和身份不确定的上下文也必须触发整条承诺的文字可靠性检查。
	hidden := wearer.Clone()
	hidden.Disposition, hidden.DispositionReason = "quarantine", "playback"
	for _, tc := range []evidenceCase{{name: "timeline", segment: timeline}, {name: "unknown", segment: unknown}, {name: "quarantine", segment: hidden}} {
		g := tc.segment.Clone()
		g.LowSupportSpans = []domain.LowSupportSpan{{Start: 1, End: 2}}
		cases = append(cases, evidenceCase{tc.name + "_low_support", g, "", true})
	}
	return cases
}

func eachEvidenceStore(t *testing.T, run func(*testing.T, *testServer)) {
	t.Helper()
	for _, backend := range []string{"memory", "pg"} {
		t.Run(backend, func(t *testing.T) {
			x := newServer(nil)
			x.Now = captureClock
			if backend == "pg" {
				x.store = pgstore.New(pgtest.New(t), captureClock)
			}
			run(t, x)
		})
	}
}

func prepareEvidence(t *testing.T, x *testServer, segments []domain.CaptureSegment) []Citation {
	t.Helper()
	if err := x.request("POST", "bl", "u", "reset", M{}, nil); err != nil {
		t.Fatal(err)
	}
	if err := x.store.Update(context.Background(), "bl", "u", func(b *domain.Book) error {
		if _, err := b.AppendCaptures("2026-10-28", "evidence", segments); err != nil {
			return err
		}
		return b.BindDay("2026-10-28", "day")
	}); err != nil {
		t.Fatal(err)
	}
	var cites []Citation
	for i, g := range segments {
		ref := domain.Ref{Seq: 1, Index: i}
		p, visible := domain.ProjectCapture(ref, g)
		if !visible {
			// 模拟旧页或伪造的宽松 cites，证明隐藏原文仍由业务侧拒绝作为依据。
			p.Speaker, p.SpeakerKey = g.Speaker, g.SpeakerKey
		}
		cites = append(cites, Citation{Ref: "#" + ref.String(), Speaker: p.Speaker, SpeakerKey: p.SpeakerKey})
	}
	return cites
}

func evidenceCreate(kind string, refs ...string) M {
	m := create("x1.0", kind, "交稿", refs...)
	m["due_date"] = "2026-10-30"
	return changes(span("#1.0", "#1.2"), []M{exchange("#1.0", "#1.2", "约定", "交稿安排", M{"match": "none"})}, M{"create": []M{m}})
}

func TestCaptureCommitmentEvidence(t *testing.T) {
	eachEvidenceStore(t, func(t *testing.T, x *testServer) {
		for _, tc := range captureEvidenceCases() {
			for _, kind := range []string{"mine", "theirs"} {
				for _, mixed := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/mixed=%t", tc.name, kind, mixed), func(t *testing.T) {
						good := domain.CaptureSegment{Speaker: "wearer", Text: "合格依据"}
						if kind == "theirs" {
							good.Speaker, good.SpeakerKey = "other", "other-2"
						}
						cites := prepareEvidence(t, x, []domain.CaptureSegment{good, tc.segment.Clone(), good.Clone()})
						refs := []string{"#1.1"}
						if mixed {
							refs = []string{"#1.0", "#1.1", "#1.0"}
						}
						out := invoke(x, "day", "file_changes", evidenceCreate(kind, refs...), cites...)
						st := x.State("bl", "u")
						if len(st.Commitments) != 1 {
							t.Fatalf("not accepted: %s", out)
						}
						want := ""
						if tc.unreliable {
							want = "引用的原话里有识别不可靠的内容"
						} else if !mixed && tc.kind != kind {
							who := "用户本人"
							if kind == "theirs" {
								who = "对方"
							}
							want = "引用的原话里没有" + who + "说的话"
						}
						if st.Commitments[0].Unverified != want || (len(st.Reminders()) == 1) != (want == "") {
							t.Fatalf("verification=%q want=%q: %s", st.Commitments[0].Unverified, want, out)
						}
						if want != "" {
							contains(t, "unverified receipt", out, "未核实："+want, "不会按期限提醒")
						}
						if tc.unreliable {
							contains(t, "unreliable reference", out, "\n  引用 #1.1 识别不可靠；", "改引那段、去掉这些引用后重交", "纪要、交谈摘要与每日总览不要把这些原话的内容写成已确认事实")
						} else {
							excludes(t, "unchanged speaker receipt", out, "识别不可靠；", "改引那段")
						}
					})
				}
			}
		}
	})
}

func TestCaptureCommitmentRevisionEvidence(t *testing.T) {
	eachEvidenceStore(t, func(t *testing.T, x *testServer) {
		for _, tc := range captureEvidenceCases() {
			for _, op := range []string{"revise", "postpone", "close"} {
				t.Run(tc.name+"/"+op, func(t *testing.T) {
					good := domain.CaptureSegment{Speaker: "wearer", Text: "合格依据"}
					cites := prepareEvidence(t, x, []domain.CaptureSegment{good, tc.segment.Clone(), good.Clone()})
					x.Seed("bl", "u", SeedData{Commitments: []SeedCommitment{{ID: "C-0001", Kind: "mine", Content: "交初稿", DueDate: "2026-10-29"}}})
					before := x.State("bl", "u")
					change := M{"id": "C-0001", "rev": 1, "refs": []string{"#1.0", "#1.1"}}
					key, verb := "revise", "修订"
					switch op {
					case "revise":
						change["content"] = "交终稿"
					case "postpone":
						change["due_date"] = "2026-10-30"
					case "close":
						key, verb = "close", "关闭"
						change["status"] = "done"
					}
					out := invoke(x, "day", "file_changes", changes(M{"day": true}, nil, M{key: []M{change}}), cites...)
					after := x.State("bl", "u")
					if tc.kind == "" || tc.unreliable {
						contains(t, "rejected revision", out, "变更集没有被接受", "引用的原话说话人不确定或识别不可靠，不能据此"+verb+"承诺")
						contains(t, "rejected reference", out, "（引用：#1.1）", "这条"+verb+"不会生效", "纪要、交谈摘要与每日总览不要写成")
						if !reflect.DeepEqual(before, after) {
							t.Fatal("rejected revision changed ledger")
						}
					} else {
						contains(t, "accepted revision", out, "已收下变更集")
						if after.Commitments[0].Rev() != 2 {
							t.Fatal("revision not recorded")
						}
					}
				})
			}
		}
	})
}

func TestCaptureCommitmentReceiptReferences(t *testing.T) {
	eachEvidenceStore(t, func(t *testing.T, x *testServer) {
		for _, op := range []string{"create", "revise", "postpone", "close"} {
			t.Run(op, func(t *testing.T) {
				bad := domain.CaptureSegment{Speaker: "wearer", Text: "甲🙂明天交稿", LowSupportSpans: []domain.LowSupportSpan{{Start: 1, End: 4}}}
				good := bad.Clone()
				good.LowSupportSpans[0].CriticalKinds = []string{"none"}
				otherBad := bad.Clone()
				otherBad.LowSupportSpans[0].CriticalKinds = []string{"date"}
				cites := prepareEvidence(t, x, []domain.CaptureSegment{bad, good, otherBad})
				cites = append(cites, Citation{Ref: "#01.0", Speaker: "wearer"})
				refs := []string{"#1.2", "#1.1", "#01.0", "#1.2"}
				if op == "create" {
					out := invoke(x, "day", "file_changes", evidenceCreate("mine", refs...), cites...)
					want := "\n  引用 #1.2、#01.0 识别不可靠；同一时段如有可靠原话支撑，改引那段、去掉这些引用后重交；没有就保留这条未核实承诺，不要删掉。纪要、交谈摘要与每日总览不要把这些原话的内容写成已确认事实。\n"
					contains(t, "only unreliable references", out, want)
					st := x.State("bl", "u")
					if len(st.Commitments) != 1 || st.Commitments[0].Unverified != "引用的原话里有识别不可靠的内容" || len(st.Reminders()) != 0 {
						t.Fatal("receipt changed stored verification")
					}
					out = invoke(x, "day", "file_changes", evidenceCreate("mine", "#1.1"), cites...)
					contains(t, "reliable replacement", out, "已核实")
					st = x.State("bl", "u")
					if len(st.Commitments) != 1 || st.Commitments[0].Unverified != "" || len(st.Reminders()) != 1 {
						t.Fatal("reliable replacement did not verify commitment")
					}
					excludes(t, "corrected receipt", out, "识别不可靠", "改引那段")
					return
				}
				x.Seed("bl", "u", SeedData{Commitments: []SeedCommitment{{ID: "C-0001", Kind: "mine", Content: "交初稿", DueDate: "2026-10-29"}}})
				before := x.State("bl", "u")
				change := M{"id": "C-0001", "rev": 1, "refs": refs}
				key, verb, effect := "revise", "修订", "已修订或已改期"
				switch op {
				case "revise":
					change["content"] = "交终稿"
				case "postpone":
					change["due_date"] = "2026-10-30"
				case "close":
					key, verb, effect = "close", "关闭", "已完成或已取消"
					change["status"] = "done"
				}
				out := invoke(x, "day", "file_changes", changes(M{"day": true}, nil, M{key: []M{change}}), cites...)
				contains(t, "only rejected references", out, "不能据此"+verb+"承诺（引用：#1.2、#01.0）。这条"+verb+"不会生效；纪要、交谈摘要与每日总览不要写成"+effect+"。")
				if !reflect.DeepEqual(before, x.State("bl", "u")) {
					t.Fatal("rejected change affected ledger")
				}
			})
		}
	})
}

func TestCaptureCommitmentPlatformChecks(t *testing.T) {
	eachEvidenceStore(t, func(t *testing.T, x *testServer) {
		for _, mode := range []string{"absent", "missing", "scope", "deleted"} {
			t.Run(mode, func(t *testing.T) {
				g := domain.CaptureSegment{Speaker: "wearer", Text: "可信原文"}
				cites := prepareEvidence(t, x, []domain.CaptureSegment{g, g.Clone(), g.Clone()})
				args := evidenceCreate("mine", "#1.1")
				switch mode {
				case "absent":
					cites = append(cites[:1], cites[2:]...)
				case "missing":
					cites[1].Missing = true
				case "scope":
					args["source"] = span("#1.0", "#1.0")
				case "deleted":
					if err := x.store.Update(context.Background(), "bl", "u", func(b *domain.Book) error {
						return b.CaptureAccess().Erase("2026-10-28", domain.Ref{Seq: 1, Index: 1}, domain.Ref{Seq: 1, Index: 1})
					}); err != nil {
						t.Fatal(err)
					}
				}
				out := invoke(x, "day", "file_changes", args, cites...)
				contains(t, "platform check", out, "变更集没有被接受")
				if len(x.State("bl", "u").Changesets) != 0 {
					t.Fatal("local evidence bypassed platform check")
				}
			})
		}
	})
}

func TestCaptureCommitmentLegacyReceipts(t *testing.T) {
	eachEvidenceStore(t, func(t *testing.T, x *testServer) {
		for _, speaker := range []string{"wearer", "other"} {
			for _, kind := range []string{"mine", "theirs"} {
				t.Run(speaker+"/"+kind, func(t *testing.T) {
					g := domain.CaptureSegment{Speaker: speaker, Text: "旧原文"}
					if speaker == "other" {
						g.SpeakerKey = "other-1"
					}
					cites := prepareEvidence(t, x, []domain.CaptureSegment{g, g.Clone(), g.Clone()})
					legacy := newServer(nil)
					legacy.Now = x.Now
					check := func(args M) string {
						t.Helper()
						want := invoke(legacy, "day", "file_changes", args, cites...)
						got := invoke(x, "day", "file_changes", args, cites...)
						if got != want {
							t.Fatalf("legacy receipt changed:\n%s\nwant:\n%s", got, want)
						}
						return got
					}
					out := check(evidenceCreate(kind, "#1.0"))
					qualified := speaker == "wearer" && kind == "mine" || speaker == "other" && kind == "theirs"
					who, text := "用户本人", "用户答应：交稿"
					if kind == "theirs" {
						who, text = "对方", "对方答应用户：交稿"
					}
					state, tail := "已核实", "新建承诺 1 条，均已核实。"
					text += "，期限 2026-10-30"
					if !qualified {
						reason := "引用的原话里没有" + who + "说的话"
						state = "未核实：" + reason + "，不会按期限提醒"
						text += "（未核实：" + reason + "）"
						tail = "1 条承诺未核实：检查 kind 与 refs，写错了就改正后用同一个 source 重交整个变更集。"
					}
					want := "已收下变更集 CS-0001（来源 #1.0–#1.2）：\n- 交谈 x1.0「约定」：不归项目\n- 承诺 C-0001（" + state + "）" + text + "\n" + tail
					if out != want {
						t.Fatalf("legacy bytes changed:\n%s\nwant:\n%s", out, want)
					}
					check(changes(M{"day": true}, nil, M{"revise": []M{{"id": "C-0001", "rev": 1, "content": "交终稿", "refs": []string{"#1.0"}}}}))
					check(changes(span("#1.1", "#1.1"), nil, M{"revise": []M{{"id": "C-0001", "rev": 2, "due_date": "2026-10-31", "refs": []string{"#1.1"}}}}))
					check(changes(span("#1.2", "#1.2"), nil, M{"close": []M{{"id": "C-0001", "rev": 3, "status": "done", "refs": []string{"#1.2"}}}}))
					if got := x.State("bl", "u").Commitments[0]; got.Rev() != 4 || got.Status != "done" {
						t.Fatal("legacy revision or close rejected")
					}
				})
			}
		}
	})
}

func TestProjectRevisionCaptureEvidence(t *testing.T) {
	eachEvidenceStore(t, func(t *testing.T, x *testServer) {
		for _, mode := range []string{"summary_only", "legacy_refs", "unknown_refs"} {
			t.Run(mode, func(t *testing.T) {
				g := domain.CaptureSegment{Speaker: "wearer", Text: "依据原文"}
				if mode == "unknown_refs" {
					g.Speaker = "unknown"
				}
				prepareEvidence(t, x, []domain.CaptureSegment{g})
				x.Seed("bl", "u", SeedData{
					Projects:    []Project{{ID: "P-1", Name: "项目"}},
					Commitments: []SeedCommitment{{ID: "C-0001", Kind: "mine", Content: "交稿", Project: "P-1"}},
					Exchanges:   []SeedExchange{{ID: "x1.0", Date: "2026-10-28", Title: "交稿", Summary: "已经交稿", Project: "P-1"}},
				})
				x.Bind("bl", "u", "P-1", "project")
				change := M{"id": "C-0001", "rev": 1, "status": "done", "exchanges": []string{"x1.0"}}
				if mode != "summary_only" {
					change["refs"] = []string{"#1.0"}
				}
				out := invoke(x, "project", "update_commitments", M{"date": "2026-10-28", "close": []M{change}})
				m := x.State("bl", "u").Commitments[0]
				if mode == "unknown_refs" {
					contains(t, "project raw evidence", out, "变更集没有被接受", "不能据此关闭承诺")
					if m.Status != "open" || m.Rev() != 1 {
						t.Fatal("project bypassed raw evidence check")
					}
				} else if m.Status != "done" || m.Rev() != 2 {
					t.Fatalf("legacy project update changed: %s", out)
				}
			})
		}
	})
}

type evidenceLookupStore struct {
	store.Store
	counts  map[domain.Ref]int
	failRef domain.Ref
	failure error
}

type evidenceLookupAccess struct {
	domain.CaptureAccess
	owner *evidenceLookupStore
}

func (a evidenceLookupAccess) Lookup(date string, ref domain.Ref) (domain.CaptureSegment, bool, error) {
	a.owner.counts[ref]++
	if ref == a.owner.failRef {
		return domain.CaptureSegment{}, false, a.owner.failure
	}
	return a.CaptureAccess.Lookup(date, ref)
}

func (s *evidenceLookupStore) Update(ctx context.Context, bl, eu string, fn func(*domain.Book) error) error {
	return s.Store.Update(ctx, bl, eu, func(b *domain.Book) error {
		b.SetCaptureAccess(evidenceLookupAccess{b.CaptureAccess(), s})
		return fn(b)
	})
}

func TestCaptureEvidenceLookupCacheAndRollback(t *testing.T) {
	eachEvidenceStore(t, func(t *testing.T, x *testServer) {
		g := domain.CaptureSegment{Speaker: "wearer", Text: "合格依据"}
		cites := prepareEvidence(t, x, []domain.CaptureSegment{g, g.Clone(), g.Clone()})
		args := evidenceCreate("mine", "#1.0", "#1.1", "#1.1")
		args["commitments"] = M{"create": []M{create("x1.0", "mine", "先检查的承诺", "#1.0"), create("x1.0", "mine", "后检查的承诺", "#1.1", "#1.1")}}
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		failure := errors.New("evidence lookup failed")
		wrapped := &evidenceLookupStore{Store: x.store, counts: map[domain.Ref]int{}, failRef: domain.Ref{Seq: 1, Index: 1}, failure: failure}
		runtime := newRuntime(wrapped, Options{AllowUnmanaged: true, Now: x.now})
		meta := mcp.Meta{"yanshi/business_line": "bl", "yanshi/end_user": "u", "yanshi/session": "day", "yanshi/captures": cites}
		before := x.State("bl", "u")
		result, err := runtime.call(context.Background(), meta, raw, (*domain.Call).FileChanges, false)
		if result != nil || !errors.Is(err, failure) || !reflect.DeepEqual(before, x.State("bl", "u")) {
			t.Fatalf("lookup failure escaped: result=%v error=%v", result, err)
		}
		if wrapped.counts[domain.Ref{Seq: 1}] != 1 || wrapped.counts[domain.Ref{Seq: 1, Index: 1}] != 1 {
			t.Fatal("failure did not follow first commitment or retried failed lookup")
		}
		wrapped.failRef, wrapped.counts = domain.Ref{}, map[domain.Ref]int{}
		result, err = runtime.call(context.Background(), meta, raw, (*domain.Call).FileChanges, false)
		if err != nil || result == nil || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "均已核实") {
			t.Fatalf("retry failed: %v %v", result, err)
		}
		for _, count := range wrapped.counts {
			if count != 1 {
				t.Fatal("repeated reference not cached")
			}
		}
		if len(wrapped.counts) != 3 || len(x.State("bl", "u").Commitments) != 2 {
			t.Fatal("retry lost references or commitments")
		}
	})
}
