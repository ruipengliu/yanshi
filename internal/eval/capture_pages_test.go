// 与 P1-⑤a 冻结的分页、说明和 schema 比较；golden 不再由内核生成。
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/capture"
	"yanshi/internal/eval/xinlifixture"
	"yanshi/internal/mcpcap"
	"yanshi/internal/model"
	"yanshi/internal/session"
)

type captureParityEnv struct {
	golden  *captureGolden
	now     time.Time
	server  mcpcap.Server
	fixture *xinlifixture.Client
	conn    *mcp.ClientSession
	sid     string
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
	if !servers[0].AttestsSpeakers || !servers[0].CiteCaptures {
		t.Fatal("xinli registration lost trust flags")
	}
	conn, err := mcp.NewClient(&mcp.Implementation{Name: "capture-parity", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: servers[0].URL, HTTPClient: &http.Client{Transport: captureFixtureTransport(servers[0].Headers)}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	e := &captureParityEnv{golden: golden, now: time.Now(), server: servers[0], fixture: f, conn: conn, sid: "capture-parity-session"}
	if err := f.BindDay(ctx, BusinessLine, "u", captureBusinessDay(e.now), e.sid); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *captureParityEnv) append(t *testing.T, segments []xinlifixture.CaptureSegment) {
	t.Helper()
	ctx := context.Background()
	e.batches++
	id := fmt.Sprintf("batch-%d", e.batches)
	_, err := e.fixture.AppendCaptures(ctx, BusinessLine, "u", captureBusinessDay(e.now), id, segments)
	if err != nil {
		t.Fatal(err)
	}
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
	if got.Text == e.golden.WidthKernel.Text {
		t.Fatal("reference-width difference disappeared")
	}
	e.golden.checkPages(t, []captureGoldenPage{got})
}

const captureGoldenPath = "testdata/capture_pages.golden.json"
const captureGoldenCommand = "从 P1-⑤a 的已核对版本恢复 testdata/capture_pages.golden.json"

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

// 测试不并行；只读取冻结基准，不提供更新入口。
var loadedCaptureGolden *captureGolden

func loadCaptureGolden(t *testing.T) *captureGolden {
	t.Helper()
	if loadedCaptureGolden != nil {
		return loadedCaptureGolden
	}
	data, err := os.ReadFile(captureGoldenPath)
	if err != nil {
		t.Fatalf("read capture golden: %v; %s", err, captureGoldenCommand)
	}
	var g captureGolden
	if err := json.Unmarshal(data, &g); err != nil {
		t.Fatal(err)
	}
	if g.Version != 1 || len(g.Pages) != 15 || g.WidthKernel.Count != 1 || g.Spec.Description == "" || len(g.Spec.Schema) == 0 {
		t.Fatalf("capture golden 尚未生成或不完整；%s", captureGoldenCommand)
	}
	if g.Pages == nil {
		g.Pages = map[string][]captureGoldenPage{}
	}
	g.Version = 1
	loadedCaptureGolden = &g
	return &g
}

func (g *captureGolden) checkPages(t *testing.T, got []captureGoldenPage) {
	t.Helper()
	want, ok := g.Pages[t.Name()]
	if !ok {
		t.Fatalf("capture golden 缺少 %s；%s", t.Name(), captureGoldenCommand)
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
	personaAfter := "0"
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

// 使用真实 Persona MCP 与内核渲染检验新字段的两种预算，不改变冻结的无引用 golden。
func TestPersonaCaptureSpeakerRefBudgets(t *testing.T) {
	for _, ref := range []string{"", "P-0012", strings.Repeat("P", 64)} {
		t.Run(fmt.Sprintf("ref%d", len(ref)), func(t *testing.T) {
			e := newCaptureParityEnv(t)
			at := time.Date(2020, 1, 1, 23, 59, 0, 0, time.UTC)
			var segments []xinlifixture.CaptureSegment
			preview := capture.RenderPage{Kind: "page", Span: capture.Ref{Seq: 1}, Done: true}
			for i := range 3 {
				when := at.Add(time.Duration(i) * time.Minute)
				segments = append(segments, xinlifixture.CaptureSegment{Speaker: "other", SpeakerKey: "other-1", SpeakerRef: ref, Text: "原话\r\n下一句", SpokenAt: when.Format(time.RFC3339), TimeZone: "UTC"})
				preview.Segments = append(preview.Segments, &v1.CaptureSegment{Ref: fmt.Sprintf("1.%d", i), Speaker: v1.Speaker_SPEAKER_OTHER, SpeakerKey: "other-1", SpeakerRef: ref, Text: "原话\r\n下一句", SpokenAt: timestamppb.New(when), TimeZone: "UTC"})
			}
			e.append(t, segments)
			// 第二段跨日：接纳之前只算首段日期头与两段正文，第二个日期头随后才计入。
			first := preview
			first.Segments = preview.Segments[:1]
			blocks, _, err := capture.Render(first, 0)
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
			boundary := chars + len([]rune("#1.1 "+model.CaptureText(preview.Segments[1])))
			for _, delta := range []int{-1, 0} {
				o := captureGoldenOptions(8000)
				o.MaxChars = boundary + delta
				pages := e.collectPages(t, true, 200, o, 10)
				want := 2
				if delta < 0 {
					want = 1
				}
				if pages[0].Count != want {
					t.Fatalf("chars delta %d: first count %d, want %d", delta, pages[0].Count, want)
				}
				read := 0
				for _, page := range pages {
					read += page.Count
				}
				if read != 3 {
					t.Fatal("character pagination lost segments")
				}
			}
			// 枚举每个 token 预算，Persona 接受的每一页都须通过平台的最终预算；同时走到续页切点。
			blocks, _, err = capture.Render(preview, 0)
			if err != nil {
				t.Fatal(err)
			}
			fullBudget := model.EstimateTokens(blocks)
			cut := false
			for budget := fullBudget; budget >= 1; budget-- {
				o := captureGoldenOptions(budget)
				res, err := e.conn.CallTool(context.Background(), &mcp.CallToolParams{Name: "captures", Arguments: map[string]any{"after": "0", "all": true}, Meta: mcp.Meta{
					"yanshi/business_line": BusinessLine, "yanshi/end_user": "u", "yanshi/session": e.sid, "yanshi/page_options": o,
				}})
				if err != nil {
					t.Fatal(err)
				}
				if res.IsError {
					break
				}
				raw, err := json.Marshal(res.Meta["yanshi/page"])
				if err != nil {
					t.Fatal(err)
				}
				p, err := capture.ParsePage(raw, o)
				if err != nil {
					t.Fatal(err)
				}
				for _, g := range p.Segments {
					if g.GetSpeakerRef() != ref {
						t.Fatal("MCP lost speaker ref")
					}
				}
				if _, _, err := capture.Render(p, budget); err != nil {
					t.Fatalf("Persona accepted budget %d: %v", budget, err)
				}
				if len(p.Segments) < 3 {
					cut = true
				}
			}
			if !cut {
				t.Fatal("token boundary not exercised")
			}
		})
	}
}

// 此夹具直接使用 SDK，仍须携带 dev 就绪登记里的凭据，与 Connector 的请求一致。
type captureFixtureTransport map[string]string

func (h captureFixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for key, value := range h {
		r.Header.Set(key, value)
	}
	return http.DefaultTransport.RoundTrip(r)
}
