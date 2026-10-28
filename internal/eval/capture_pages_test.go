package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/capability"
	"yanshi/internal/capture"
	"yanshi/internal/clock"
	"yanshi/internal/eval/xinlifixture"
	"yanshi/internal/eventlog"
	"yanshi/internal/eventlog/memlog"
	"yanshi/internal/ids"
	"yanshi/internal/lifecycle"
	"yanshi/internal/mcpcap"
	"yanshi/internal/model"
	"yanshi/internal/moderation"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/workqueue/memqueue"
)

type captureParityEnv struct {
	server  mcpcap.Server
	fixture *xinlifixture.Client
	conn    *mcp.ClientSession
	svc     *service.Service
	log     *memlog.Log
	clk     *clock.Fake
	agent   *agentdef.Def
	sid     string
	refs    map[string]string
	batches int
}

func newCaptureParityEnv(t *testing.T) *captureParityEnv {
	t.Helper()
	ctx := context.Background()
	f, servers, err := xinlifixture.Start(ctx, defaultXinliBin, BusinessLine)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if !servers[0].AttestsSpeakers || !servers[0].CiteCaptures || servers[0].Acks {
		t.Fatal("xinli registration lost trust flags")
	}
	conn, err := mcp.NewClient(&mcp.Implementation{Name: "capture-parity", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: servers[0].URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	clk := clock.NewFake(time.Now())
	def := &agentdef.Def{Name: "day", Version: "1", Model: "echo/any", Streams: []agentdef.Stream{{Name: session.StreamTranscript, ExchangeGap: 5 * time.Minute}}}
	agents, err := agentdef.NewRegistry(def)
	if err != nil {
		t.Fatal(err)
	}
	log, q := memlog.New(), memqueue.New(clk)
	st := &session.Store{Log: log, Waking: log.Waking(q), IDs: ids.Sequential("capture-parity"), Clock: clk}
	svc := &service.Service{Store: st, Agents: agents, Index: lifecycle.NewMemIndex(), Moderator: moderation.Mock{}}
	sid, err := svc.Create(ctx, service.CreateRequest{BusinessLine: BusinessLine, EndUser: "u", Agent: "day"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.BindDay(ctx, BusinessLine, "u", "2026-10-27", sid); err != nil {
		t.Fatal(err)
	}
	return &captureParityEnv{server: servers[0], fixture: f, conn: conn, svc: svc, log: log, clk: clk, agent: def, sid: sid, refs: map[string]string{}}
}

func (e *captureParityEnv) append(t *testing.T, segments []xinlifixture.CaptureSegment) {
	t.Helper()
	ctx := context.Background()
	e.batches++
	id := fmt.Sprintf("batch-%d", e.batches)
	refs, err := e.fixture.AppendCaptures(ctx, BusinessLine, "u", "2026-10-27", id, segments)
	if err != nil {
		t.Fatal(err)
	}
	var gs []*v1.CaptureSegment
	for _, s := range segments {
		g := &v1.CaptureSegment{Speaker: v1.Speaker_SPEAKER_WEARER, SpeakerKey: s.SpeakerKey, Text: s.Text, TimeZone: s.TimeZone}
		if s.Speaker == "other" {
			g.Speaker = v1.Speaker_SPEAKER_OTHER
		}
		if s.SpokenAt != "" {
			at, err := time.Parse(time.RFC3339Nano, s.SpokenAt)
			if err != nil {
				t.Fatal(err)
			}
			g.SpokenAt = timestamppb.New(at)
		}
		gs = append(gs, g)
	}
	if _, err := e.svc.SubmitCaptures(ctx, e.sid, session.StreamTranscript, id, gs); err != nil {
		t.Fatal(err)
	}
	events, err := eventlog.ReadAll(ctx, e.log, e.sid, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.GetCaptureAppended().GetCaptureId() == id {
			for i, r := range refs {
				e.refs[fmt.Sprintf("%d.%d", event.GetSeq(), i)] = r
			}
			return
		}
	}
	t.Fatal("submitted batch absent from log")
}

func (e *captureParityEnv) page(t *testing.T, after string, all bool, limit int, o capture.PageOptions) (capture.StructuredPage, json.RawMessage) {
	t.Helper()
	if !e.server.AttestsSpeakers {
		t.Fatal("test must enable attested pages")
	}
	res, err := e.conn.CallTool(context.Background(), &mcp.CallToolParams{Name: "captures", Arguments: map[string]any{"after": after, "all": all, "limit": limit}, Meta: mcp.Meta{
		"yanshi/business_line": BusinessLine, "yanshi/end_user": "u", "yanshi/session": e.sid, "yanshi/page_options": o,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("MCP captures error: %+v", res.Content)
	}
	if len(res.Content) != 1 || res.Content[0].(*mcp.TextContent).Text != "旁听页见结构化元数据" {
		t.Fatal("raw text escaped structured metadata")
	}
	raw, err := json.Marshal(res.Meta["yanshi/page"])
	if err != nil {
		t.Fatal(err)
	}
	var p capture.StructuredPage
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	return p, raw
}

var captureParityRef = regexp.MustCompile(`[0-9]+\.[0-9]+`)

// 只在合成文本中映射编号，正文不含 N.M；两边编号位数相同，预算也一致。
func (e *captureParityEnv) mapRefs(s string) string {
	return captureParityRef.ReplaceAllStringFunc(s, func(r string) string {
		if mapped, ok := e.refs[r]; ok {
			return mapped
		}
		return r
	})
}
func captureParityText(blocks []*v1.ContentBlock) string {
	return model.Text(model.ExpandCaptures(blocks))
}

func TestPersonaCapturePageParity(t *testing.T) {
	e := newCaptureParityEnv(t)
	segment := func(at time.Time, text string, other bool) xinlifixture.CaptureSegment {
		s := xinlifixture.CaptureSegment{Speaker: "wearer", Text: text, SpokenAt: at.Format(time.RFC3339Nano), TimeZone: "Asia/Shanghai"}
		if other {
			s.Speaker, s.SpeakerKey = "other", "other-1"
		}
		return s
	}
	// 同一时段跨当地午夜与批次；长时段分别被 limit、字符数和 token 预算切开。
	midnight := time.Date(2020, 1, 1, 15, 59, 0, 0, time.UTC)
	e.append(t, []xinlifixture.CaptureSegment{segment(midnight, "我来提交。\n收到后确认。", false), segment(midnight.Add(2*time.Minute), "好的\r请确认。", true)})
	var long []xinlifixture.CaptureSegment
	for i := range 36 {
		long = append(long, segment(midnight.Add(time.Duration(i+3)*time.Minute), strings.Repeat("事项", 200), i%2 == 1))
	}
	e.append(t, long)
	// 没有时间时按页分时段；最后补上带时间的一段，使后一个新时段的停顿可判定。
	e.append(t, []xinlifixture.CaptureSegment{{Speaker: "wearer", Text: "没有时间的原话"}, {Speaker: "other", SpeakerKey: "other-2", Text: "没有时区", SpokenAt: midnight.Format(time.RFC3339Nano)}, segment(e.clk.Now().Add(-time.Hour), "之前的时段末尾", false)})
	var recent []xinlifixture.CaptureSegment
	for i := range 4 {
		recent = append(recent, segment(e.clk.Now().Add(time.Duration(i-5)*10*time.Second), "刚说完的话", i%2 == 1))
	}
	e.append(t, recent)
	for _, budget := range []int{8000, 700} {
		for _, limit := range []int{1, 3, 200} {
			for _, all := range []bool{false, true} {
				t.Run(fmt.Sprintf("budget%d/limit%d/all%t", budget, limit, all), func(t *testing.T) {
					agent := *e.agent
					agent.Context.MaxToolResult = budget
					inv := capability.Invocation{SessionID: e.sid, Agent: &agent}
					o := capture.OptionsFor(inv)
					kernelAfter, personaAfter := "0", "0"
					pages, read, inProgress := 0, 0, false
					for ; pages < 200; pages++ {
						args, _ := json.Marshal(map[string]any{"after": kernelAfter, "all": all, "limit": limit})
						inv.Arguments = string(args)
						result, err := capture.New(e.log, e.clk).InvokeResult(context.Background(), inv)
						if err != nil {
							t.Fatal(err)
						}
						wire, raw := e.page(t, personaAfter, all, limit, o)
						parsed, err := capture.ParsePage(raw, o)
						if err != nil {
							t.Fatalf("page %d parse: %v", pages, err)
						}
						blocks, next, err := capture.Render(parsed, budget)
						if err != nil {
							t.Fatal(err)
						}
						want := e.mapRefs(captureParityText(result.Content))
						got := captureParityText(blocks)
						if got != want {
							t.Fatalf("page %d differs\ngot %s\nwant %s", pages, got, want)
						}
						if result.Read != nil && e.mapRefs(result.Read.GetAfter()) != next.String() {
							t.Fatal("cursor mapping differs")
						}
						read += len(wire.Segments)
						inProgress = wire.Kind == "in_progress"
						if wire.Kind != "page" || wire.Done {
							break
						}
						if result.Read == nil {
							t.Fatal("missing kernel cursor")
						}
						kernelAfter, personaAfter = result.Read.GetAfter(), next.String()
					}
					if pages == 200 {
						t.Fatal("did not terminate")
					}
					if all && (read != 45 || inProgress) {
						t.Fatalf("all read %d", read)
					}
					if !all && (!inProgress || read != 41) {
						t.Fatalf("pending read %d, in_progress=%t", read, inProgress)
					}
				})
			}
		}
	}
}

// 第 5 步删除内置工具后改为与冻结的 golden 比较。
func TestPersonaCapturesSpecParity(t *testing.T) {
	e := newCaptureParityEnv(t)
	want := capture.New(e.log, e.clk).Spec()
	for tool, err := range e.conn.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		if tool.Name != "captures" {
			continue
		}
		if tool.Description != want.Description {
			t.Fatal("captures description differs")
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		// 线上 JSON 对象重排不改变 schema；字符串值、数值和字段都须一致。
		var gotSchema, wantSchema any
		if json.Unmarshal(raw, &gotSchema) != nil || json.Unmarshal(want.InputSchema, &wantSchema) != nil || !reflect.DeepEqual(gotSchema, wantSchema) {
			t.Fatal("captures schema differs")
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint {
			t.Fatal("captures annotations differ")
		}
		return
	}
	t.Fatal("captures missing")
}

// 跨日日期头在接纳片段后计入，两条路径须接受同样的页并继续读到末尾。
func TestPersonaCaptureCharacterBoundary(t *testing.T) {
	e := newCaptureParityEnv(t)
	at := time.Date(2020, 1, 1, 23, 59, 0, 0, time.UTC)
	var segs []xinlifixture.CaptureSegment
	preview := capture.RenderPage{Kind: "page", Span: capture.Ref{Seq: 2}, Done: true}
	for i := range 3 {
		text := strings.Repeat("字", 1800)
		segs = append(segs, xinlifixture.CaptureSegment{Speaker: "wearer", Text: text, SpokenAt: at.Format(time.RFC3339), TimeZone: "UTC"})
		preview.Segments = append(preview.Segments, &v1.CaptureSegment{Ref: fmt.Sprintf("2.%d", i), Speaker: v1.Speaker_SPEAKER_WEARER, Text: text, SpokenAt: timestamppb.New(at), TimeZone: "UTC"})
	}
	blocks, _, err := capture.Render(preview, 0)
	if err != nil {
		t.Fatal(err)
	}
	chars := 0
	for _, b := range blocks {
		if g := b.GetCapture(); g != nil {
			chars += len([]rune(model.CaptureText(g)))
		} else if text := b.GetText().GetText(); strings.HasPrefix(text, "——") || strings.HasPrefix(text, "#") {
			chars += len([]rune(text))
		}
	}
	nextAt := at.Add(2 * time.Minute)
	g := &v1.CaptureSegment{Speaker: v1.Speaker_SPEAKER_WEARER, SpokenAt: timestamppb.New(nextAt), TimeZone: "UTC"}
	textLen := capture.MaxChars - chars - len([]rune("#2.3 "+model.CaptureText(g)))
	if textLen <= 0 || textLen > session.MaxCaptureText {
		t.Fatal("invalid boundary fixture")
	}
	segs = append(segs, xinlifixture.CaptureSegment{Speaker: "wearer", Text: strings.Repeat("字", textLen), SpokenAt: nextAt.Format(time.RFC3339), TimeZone: "UTC"}, xinlifixture.CaptureSegment{Speaker: "wearer", Text: "下一页", SpokenAt: nextAt.Format(time.RFC3339), TimeZone: "UTC"})
	e.append(t, segs)
	assertCaptureBoundaryPages(t, e, 200, 4, 5)
}

// 平台接受上限为 500；模型明确请求 201 时可读到第 201 段，下一页接着读。
func TestPersonaCaptureLimitBoundary(t *testing.T) {
	e := newCaptureParityEnv(t)
	segs := make([]xinlifixture.CaptureSegment, 200)
	for i := range segs {
		segs[i] = xinlifixture.CaptureSegment{Speaker: "wearer", Text: "字"}
	}
	e.append(t, segs)
	e.append(t, segs[:2])
	assertCaptureBoundaryPages(t, e, 201, 201, 202)
}

func assertCaptureBoundaryPages(t *testing.T, e *captureParityEnv, limit, firstCount, total int) {
	t.Helper()
	agent := *e.agent
	agent.Context.MaxToolResult = 8000
	inv := capability.Invocation{SessionID: e.sid, Agent: &agent}
	opts := capture.OptionsFor(inv)
	kernelAfter, personaAfter := "0", "0"
	read := 0
	for i := 0; i < 10; i++ {
		args, _ := json.Marshal(map[string]any{"after": kernelAfter, "all": true, "limit": limit})
		inv.Arguments = string(args)
		want, err := capture.New(e.log, e.clk).InvokeResult(context.Background(), inv)
		if err != nil {
			t.Fatal(err)
		}
		wire, raw := e.page(t, personaAfter, true, limit, opts)
		parsed, err := capture.ParsePage(raw, opts)
		if err != nil {
			t.Fatal(err)
		}
		got, next, err := capture.Render(parsed, opts.MaxToolResult)
		if err != nil {
			t.Fatal(err)
		}
		if captureParityText(got) != e.mapRefs(captureParityText(want.Content)) {
			t.Fatalf("page %d differs", i)
		}
		if want.Read == nil || next.String() != e.mapRefs(want.Read.GetAfter()) {
			t.Fatal("cursor differs")
		}
		if i == 0 && len(wire.Segments) != firstCount {
			t.Fatalf("first page: %d", len(wire.Segments))
		}
		read += len(wire.Segments)
		if wire.Done {
			if read != total || i == 0 {
				t.Fatalf("read %d on page %d", read, i)
			}
			return
		}
		kernelAfter, personaAfter = want.Read.GetAfter(), next.String()
	}
	t.Fatal("boundary pages did not terminate")
}

// 引用位数不同造成的切页差异是已知且可接受的：模型看到的引用本来就不同。
// 映射发生在渲染之后，无法撤销已经发生的切页差异。
func TestPersonaCaptureReferenceWidthBoundary(t *testing.T) {
	e := newCaptureParityEnv(t)
	one := []xinlifixture.CaptureSegment{{Speaker: "wearer", Text: "字"}}
	for range 8 {
		e.append(t, one)
	}
	e.append(t, append(append([]xinlifixture.CaptureSegment(nil), one...), one...))
	var last capture.Ref
	for kernel, persona := range e.refs {
		if persona == "8.0" {
			var ok bool
			last, ok = capture.ParseRef(kernel)
			if !ok {
				t.Fatal("bad kernel ref")
			}
		}
	}
	if last.Seq != 9 {
		t.Fatalf("fixture sequence changed: %v", last)
	}
	g := &v1.CaptureSegment{Speaker: v1.Speaker_SPEAKER_WEARER, Text: "字"}
	maxChars := 2 * len([]rune("#9.0 "+model.CaptureText(g)))
	opts := capture.PageOptions{Limit: 200, MaxChars: maxChars, GapMS: 300000}
	wire, raw := e.page(t, "8.0", true, 200, opts)
	parsed, err := capture.ParsePage(raw, opts)
	if err != nil {
		t.Fatal(err)
	}
	rendered, _, err := capture.Render(parsed, 0)
	if err != nil {
		t.Fatal(err)
	}
	kernel, _, err := capture.Page(context.Background(), e.log, e.sid, capture.Cursor{Last: last}, capture.Options{Limit: 200, MaxChars: maxChars, Gap: 5 * time.Minute, All: true})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, b := range kernel {
		if b.GetCapture() != nil {
			n++
		}
	}
	if len(wire.Segments) != 2 || n != 1 || captureParityText(rendered) == e.mapRefs(captureParityText(kernel)) {
		t.Fatal("expected width-dependent page boundary")
	}
}
