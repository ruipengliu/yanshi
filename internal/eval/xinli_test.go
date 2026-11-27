package eval

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"yanshi/internal/eval/xinlifixture"
)

// 旁听按说话时间约每分钟一批，每批的他人按第一次出现的顺序重新编号：同一个人跨批换了编号，与真实的 HostApp 一样。
func TestCaptureBatches(t *testing.T) {
	base := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	seg := func(sec int, key string) xinlifixture.CaptureSegment {
		g := xinlifixture.CaptureSegment{Speaker: "wearer", SpokenAt: base.Add(time.Duration(sec) * time.Second).Format(time.RFC3339Nano)}
		if key != "" {
			g.Speaker, g.SpeakerKey = "other", key
		}
		return g
	}
	batches := captureBatches([]xinlifixture.CaptureSegment{seg(0, "li"), seg(10, ""), seg(30, "wang"), seg(59, "li"), seg(60, "wang"), seg(200, "li")})
	var got []string
	for _, b := range batches {
		var keys []string
		for _, g := range b {
			keys = append(keys, g.SpeakerKey)
		}
		got = append(got, strings.Join(keys, ","))
	}
	if want := []string{"other-1,,other-2,other-1", "other-1", "other-1"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("batches = %q, want %q", got, want)
	}
	// 无时间只按 100 段切分；unknown 与 timeline 不参与 other 编号，也不在此过滤隐藏片段。
	untimed := make([]xinlifixture.CaptureSegment, 103)
	for i := range untimed {
		untimed[i] = xinlifixture.CaptureSegment{Speaker: "other", SpeakerKey: "li", Text: "原话"}
	}
	untimed[0] = xinlifixture.CaptureSegment{Speaker: "unknown", Text: "不确定"}
	untimed[1] = xinlifixture.CaptureSegment{Speaker: "wearer", SpeakerGrade: "timeline", Text: "时间线"}
	untimed[2].Disposition, untimed[2].DispositionReason = "quarantine", "playback"
	untimed[100].SpeakerKey, untimed[100].SpeakerRef = "wang", "P-wang"
	parts := captureBatches(untimed)
	if len(parts) != 2 || len(parts[0]) != 100 || len(parts[1]) != 3 || parts[0][2].SpeakerKey != "other-1" || parts[1][0].SpeakerKey != "other-1" || parts[1][1].SpeakerKey != "other-2" || parts[1][0].SpeakerRef != "P-wang" {
		t.Fatalf("untimed batches = %+v", parts)
	}
	if untimed[2].SpeakerKey != "li" || parts[0][0].Speaker != "unknown" || parts[0][1].SpeakerGrade != "timeline" || parts[0][2].Disposition != "quarantine" {
		t.Fatal("batching changed source data or capture metadata")
	}
}

// 碎片率与纯度按交谈的范围计算：交谈的主题是范围内最多的主题；关联几个项目的交谈对每个项目算作那个项目自己的主题。
func TestCheckGroupingByRange(t *testing.T) {
	topics := map[string]string{"s/2.0": "okr", "s/2.1": "okr", "s/2.2": "kangning", "s/3.0": "okr", "s/4.0": "kangning", "s/5.0": "", "s/6.0": "chen", "s/7.0": "kangning"}
	link := func(ps ...string) []xinlifixture.Link {
		var out []xinlifixture.Link
		for _, p := range ps {
			out = append(out, xinlifixture.Link{Match: "existing", Project: p})
		}
		return out
	}
	st := &xinlifixture.State{Exchanges: []xinlifixture.Exchange{
		{ID: "x2.0", Session: "s", From: "#2.0", To: "#2.2", Projects: link("P-okr", "P-kn")}, // 黄总一次谈两件事
		{ID: "x3.0", Session: "s", From: "#3.0", To: "#3.0", Projects: link("P-okr")},
		{ID: "x4.0", Session: "s", From: "#4.0", To: "#4.0", Projects: link("P-kn")},
		{ID: "x5.0", Session: "s", From: "#5.0", To: "#6.0", Projects: link("P-kn")}, // 闲聊夹着老陈：老陈的事错归进康宁
		{ID: "x7.0", Session: "s", From: "#7.0", To: "#7.0", Projects: link("P-kn")},
		{ID: "x9.0", Session: "seed", Projects: link("P-kn")}, // 预置的交谈没有范围，不计
	}}
	var out []string
	add := func(name string, ok bool, detail string) {
		out = append(out, fmt.Sprintf("%s=%v %s", name, ok, detail))
	}
	purity, frag := 0.7, 1.4
	checkGrouping(add, &GroupingExpect{Purity: &purity, Fragmentation: &FloatRange{Max: &frag}}, st, topics)
	// okr → P-okr；kangning → P-kn；chen → P-kn：碎片率 1；P-kn 中 kangning 占 3/4，总纯度 (2+3)/6。
	if got := strings.Join(out, "\n"); !strings.Contains(got, "xinli fragmentation=true fragmentation 1.00 over 3 topics, purity 0.83 over 6") ||
		!strings.Contains(got, "xinli purity=true") {
		t.Fatalf("grouping:\n%s", got)
	}
}

// 在 make check 中检查用例和数据引用，避免等到真实模型评测时才发现错误。
func TestShippedSuiteLoads(t *testing.T) {
	for name, dir := range map[string]string{
		"kernel": "../../evals",
		"xinli":  "../../apps/xinli/evals",
	} {
		t.Run(name, func(t *testing.T) {
			cases, err := LoadSuite(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(cases) == 0 {
				t.Fatalf("no cases in %s", dir)
			}
		})
	}
}
