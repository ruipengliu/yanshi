// 冻结内核分页、说明与 schema：
// go test ./internal/eval -run 'TestPersonaCapture' -update-capture-golden
// 第 5 步删除内核路径时，一并删除更新标志与标注的生成专用函数，保留 golden。
// 引用位数边界原本就不相等：冻结 Persona 页，并另存内核页作为差异依据。
package eval

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
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
	golden  *captureGolden
	now     time.Time
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
	golden := loadCaptureGolden(t)
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
	e := &captureParityEnv{golden: golden, now: time.Now(), server: servers[0], fixture: f, conn: conn, sid: "capture-parity-session", refs: map[string]string{}}
	if *updateCaptureGolden {
		e.initKernel(t)
	}
	if err := f.BindDay(ctx, BusinessLine, "u", "2026-10-27", e.sid); err != nil {
		t.Fatal(err)
	}
	return e
}

// 仅生成时装配内核；第 5 步连同更新标志删除。
func (e *captureParityEnv) initKernel(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	e.clk = clock.NewFake(e.now)
	def := &agentdef.Def{Name: "day", Version: "1", Model: "echo/any", Streams: []agentdef.Stream{{Name: session.StreamTranscript, ExchangeGap: 5 * time.Minute}}}
	agents, err := agentdef.NewRegistry(def)
	if err != nil {
		t.Fatal(err)
	}
	log, q := memlog.New(), memqueue.New(e.clk)
	st := &session.Store{Log: log, Waking: log.Waking(q), IDs: ids.Sequential("capture-parity"), Clock: e.clk}
	svc := &service.Service{Store: st, Agents: agents, Index: lifecycle.NewMemIndex(), Moderator: moderation.Mock{}}
	sid, err := svc.Create(ctx, service.CreateRequest{BusinessLine: BusinessLine, EndUser: "u", Agent: "day"})
	if err != nil {
		t.Fatal(err)
	}
	e.svc, e.log, e.agent, e.sid = svc, log, def, sid
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
	if *updateCaptureGolden {
		e.appendKernel(t, id, segments, refs)
	}
}

// 仅生成时写入内核日志并建立引用映射；第 5 步连同更新标志删除。
func (e *captureParityEnv) appendKernel(t *testing.T, id string, segments []xinlifixture.CaptureSegment, refs []string) {
	t.Helper()
	ctx := context.Background()
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

// 仅生成时在合成文本中映射编号，正文不含 N.M；第 5 步连同 captureParityRef 删除。
// 普通用例两边编号位数相同，引用位数边界单独验证允许的差异。
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
	e.append(t, []xinlifixture.CaptureSegment{{Speaker: "wearer", Text: "没有时间的原话"}, {Speaker: "other", SpeakerKey: "other-2", Text: "没有时区", SpokenAt: midnight.Format(time.RFC3339Nano)}, segment(midnight.Add(2*time.Hour), "之前的时段末尾", false)})
	var recent []xinlifixture.CaptureSegment
	for i := range 4 {
		recent = append(recent, segment(midnight.Add(3*time.Hour+time.Duration(i)*10*time.Second), "末尾时段的话", i%2 == 1))
	}
	e.append(t, recent)
	for _, budget := range []int{8000, 700} {
		for _, limit := range []int{1, 3, 200} {
			for _, all := range []bool{false, true} {
				t.Run(fmt.Sprintf("budget%d/limit%d/all%t", budget, limit, all), func(t *testing.T) {
					opts := captureGoldenOptions(budget)
					pages := e.collectPages(t, all, limit, opts, 200)
					read := 0
					for _, page := range pages {
						read += page.Count
					}
					if read != 45 || pages[len(pages)-1].Kind != "page" {
						t.Fatalf("read %d, last kind %s", read, pages[len(pages)-1].Kind)
					}
					e.golden.checkPages(t, pages)
				})
			}
		}
	}
	t.Run("in_progress", assertCaptureInProgress)
}

// 说明逐字冻结，schema 忽略 JSON 对象的键顺序。
func TestPersonaCapturesSpecParity(t *testing.T) {
	e := newCaptureParityEnv(t)
	want := e.golden.Spec
	if *updateCaptureGolden {
		want = e.kernelSpec()
	}
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
		if json.Unmarshal(raw, &gotSchema) != nil || json.Unmarshal(want.Schema, &wantSchema) != nil || !reflect.DeepEqual(gotSchema, wantSchema) {
			t.Fatal("captures schema differs")
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || !tool.Annotations.IdempotentHint {
			t.Fatal("captures annotations differ")
		}
		e.golden.Spec = want
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
	pages := e.collectPages(t, true, limit, captureGoldenOptions(8000), 10)
	read := 0
	for _, page := range pages {
		read += page.Count
	}
	if len(pages) < 2 || pages[0].Count != firstCount || read != total || !pages[len(pages)-1].Done {
		t.Fatalf("boundary pages: first=%d, total=%d, pages=%d", pages[0].Count, read, len(pages))
	}
	e.golden.checkPages(t, pages)
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
	g := &v1.CaptureSegment{Speaker: v1.Speaker_SPEAKER_WEARER, Text: "字"}
	maxChars := 2 * len([]rune("#9.0 "+model.CaptureText(g)))
	opts := capture.PageOptions{Limit: 200, MaxChars: maxChars, GapMS: 300000}
	wire, raw := e.page(t, "8.0", true, 200, opts)
	parsed, err := capture.ParsePage(raw, opts)
	if err != nil {
		t.Fatal(err)
	}
	rendered, next, err := capture.Render(parsed, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := captureGoldenPage{Text: captureParityText(rendered), Next: next.String(), Kind: wire.Kind, Count: len(wire.Segments), Done: wire.Done}
	if got.Count != 2 {
		t.Fatalf("reference width page: %d", got.Count)
	}
	if *updateCaptureGolden {
		e.golden.WidthKernel = e.kernelWidthPage(t, opts, got)
	}
	e.golden.checkPages(t, []captureGoldenPage{got})
}

// 引用位数边界本就不同；仅生成时验证并冻结内核一段、Persona 两段的差异。
// 第 5 步连同更新标志删除。
func (e *captureParityEnv) kernelWidthPage(t *testing.T, opts capture.PageOptions, got captureGoldenPage) captureGoldenPage {
	t.Helper()
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
	kernel, next, err := capture.Page(context.Background(), e.log, e.sid, capture.Cursor{Last: last}, capture.Options{Limit: 200, MaxChars: opts.MaxChars, Gap: 5 * time.Minute, All: true})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, b := range kernel {
		if b.GetCapture() != nil {
			n++
		}
	}
	if n != 1 || got.Text == e.mapRefs(captureParityText(kernel)) {
		t.Fatal("expected width-dependent page boundary")
	}
	return captureGoldenPage{Text: e.mapRefs(captureParityText(kernel)), Next: e.mapRefs(next.String()), Kind: "page", Count: n}
}

var updateCaptureGolden = flag.Bool("update-capture-golden", false, "从当前内核生成旁听分页 golden；第 5 步删除此标志")

const captureGoldenPath = "testdata/capture_pages.golden.json"
const captureGoldenCommand = "go test ./internal/eval -run 'TestPersonaCapture' -update-capture-golden"

type captureGoldenPage struct {
	Text  string `json:"text"`
	Next  string `json:"next"`
	Kind  string `json:"kind"`
	Count int    `json:"count"`
	Done  bool   `json:"done"`
}

type captureGoldenSpec struct {
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
}

type captureGolden struct {
	Version     int                            `json:"version"`
	Pages       map[string][]captureGoldenPage `json:"pages"`
	Spec        captureGoldenSpec              `json:"spec"`
	WidthKernel captureGoldenPage              `json:"width_kernel"`
}

// 测试不并行；整个进程成功后才写文件，避免中途失败留下半份基准。
var loadedCaptureGolden *captureGolden

func loadCaptureGolden(t *testing.T) *captureGolden {
	t.Helper()
	if loadedCaptureGolden != nil {
		return loadedCaptureGolden
	}
	data, err := os.ReadFile(captureGoldenPath)
	if err != nil {
		t.Fatalf("read capture golden: %v; 运行 %s", err, captureGoldenCommand)
	}
	var g captureGolden
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	if !*updateCaptureGolden && (g.Version != 1 || len(g.Pages) != 15 || g.WidthKernel.Count != 1 || g.Spec.Description == "" || len(g.Spec.Schema) == 0) {
		t.Fatalf("capture golden 尚未生成或不完整；运行 %s", captureGoldenCommand)
	}
	if g.Pages == nil {
		g.Pages = map[string][]captureGoldenPage{}
	}
	g.Version = 1
	loadedCaptureGolden = &g
	return &g
}

// 仅生成时写文件；第 5 步连同更新标志及 TestMain 中的调用删除。
func writeCaptureGolden() error {
	if !*updateCaptureGolden || loadedCaptureGolden == nil {
		return nil
	}
	g := loadedCaptureGolden
	// 完整命令应生成 12 个参数组合与三个边界；拒绝从占位生成不完整的基准。
	if len(g.Pages) != 15 || g.Spec.Description == "" || len(g.Spec.Schema) == 0 || g.WidthKernel.Count != 1 {
		return fmt.Errorf("capture golden 不完整；运行 %s", captureGoldenCommand)
	}
	data, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(captureGoldenPath, append(data, '\n'), 0644)
}

func (g *captureGolden) checkPages(t *testing.T, got []captureGoldenPage) {
	t.Helper()
	if *updateCaptureGolden {
		g.Pages[t.Name()] = got
		return
	}
	want, ok := g.Pages[t.Name()]
	if !ok {
		t.Fatalf("capture golden 缺少 %s；运行 %s", t.Name(), captureGoldenCommand)
	}
	if len(got) != len(want) {
		t.Fatalf("page count: got %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("page %d differs\ngot %+v\nwant %+v", i, got[i], want[i])
		}
	}
}

func captureGoldenOptions(budget int) capture.PageOptions {
	return capture.PageOptions{Limit: 500, MaxChars: 6000, GapMS: 300000, MaxToolResult: budget}
}

func (e *captureParityEnv) collectPages(t *testing.T, all bool, limit int, opts capture.PageOptions, maxPages int) []captureGoldenPage {
	t.Helper()
	kernelAfter, personaAfter := "0", "0"
	var pages []captureGoldenPage
	for i := 0; i < maxPages; i++ {
		wire, raw := e.page(t, personaAfter, all, limit, opts)
		parsed, err := capture.ParsePage(raw, opts)
		if err != nil {
			t.Fatalf("page %d parse: %v", i, err)
		}
		blocks, next, err := capture.Render(parsed, opts.MaxToolResult)
		if err != nil {
			t.Fatal(err)
		}
		page := captureGoldenPage{Text: captureParityText(blocks), Next: next.String(), Kind: wire.Kind, Count: len(wire.Segments), Done: wire.Done}
		if *updateCaptureGolden {
			page.Text, page.Next, kernelAfter = e.compareKernelPage(t, kernelAfter, all, limit, opts, page)
		}
		pages = append(pages, page)
		if wire.Kind != "page" || wire.Done {
			return pages
		}
		if next.String() == personaAfter {
			t.Fatal("cursor did not advance")
		}
		personaAfter = next.String()
	}
	t.Fatal("pages did not terminate")
	return nil
}

// 仅生成时读取内核并逐字比较；返回映射后的内核文字与游标作为 golden 真值。
// 第 5 步连同更新标志删除。
func (e *captureParityEnv) compareKernelPage(t *testing.T, after string, all bool, limit int, opts capture.PageOptions, got captureGoldenPage) (string, string, string) {
	t.Helper()
	agent := *e.agent
	agent.Context.MaxToolResult = opts.MaxToolResult
	args, err := json.Marshal(map[string]any{"after": after, "all": all, "limit": limit})
	if err != nil {
		t.Fatal(err)
	}
	inv := capability.Invocation{SessionID: e.sid, Agent: &agent, Arguments: string(args)}
	if capture.OptionsFor(inv) != opts {
		t.Fatal("kernel page options differ")
	}
	result, err := capture.New(e.log, e.clk).InvokeResult(context.Background(), inv)
	if err != nil {
		t.Fatal(err)
	}
	next := "0"
	if result.Read != nil {
		next = result.Read.GetAfter()
	} else if got.Kind == "page" {
		t.Fatal("missing kernel cursor")
	}
	text, mapped := e.mapRefs(captureParityText(result.Content)), e.mapRefs(next)
	if got.Text != text || got.Next != mapped {
		t.Fatalf("kernel page differs\ngot %s (%s)\nwant %s (%s)", got.Text, got.Next, text, mapped)
	}
	return text, mapped, next
}

// 仅生成时读取内置说明与 schema；第 5 步连同更新标志删除。
func (e *captureParityEnv) kernelSpec() captureGoldenSpec {
	spec := capture.New(e.log, e.clk).Spec()
	return captureGoldenSpec{Description: spec.Description, Schema: spec.InputSchema}
}

// 相对当前时间的数据不写 golden，避免日期头、当地时刻与星期跨日期变化。
// 完整固定时间矩阵冻结全部文字；这里保留之前的时段可读、最后四段不可提前读的覆盖。
func assertCaptureInProgress(t *testing.T) {
	e := newCaptureParityEnv(t)
	e.append(t, []xinlifixture.CaptureSegment{{Speaker: "wearer", Text: "之前的时段", SpokenAt: "2020-01-01T00:00:00Z", TimeZone: "Asia/Shanghai"}})
	var recent []xinlifixture.CaptureSegment
	for i := range 4 {
		recent = append(recent, xinlifixture.CaptureSegment{Speaker: "wearer", Text: "刚说完的话", SpokenAt: e.now.Add(time.Duration(i-5) * 10 * time.Second).Format(time.RFC3339Nano), TimeZone: "Asia/Shanghai"})
	}
	e.append(t, recent)
	for _, budget := range []int{8000, 700} {
		for _, limit := range []int{1, 3, 200} {
			for _, all := range []bool{false, true} {
				pages := e.collectPages(t, all, limit, captureGoldenOptions(budget), 10)
				read := 0
				for _, page := range pages {
					read += page.Count
				}
				last := pages[len(pages)-1]
				if all && (read != 5 || !last.Done) || !all && (read != 1 || last.Kind != "in_progress" || last.Count != 0) {
					t.Fatalf("budget=%d limit=%d all=%t: read=%d last=%+v", budget, limit, all, read, last)
				}
			}
		}
	}
}
