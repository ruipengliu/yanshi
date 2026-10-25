package eval

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/xinlimock"
)

// 旁听按说话时间约每分钟一批，每批的他人按第一次出现的顺序重新编号：同一个人跨批换了编号，与真实的 HostApp 一样。
func TestCaptureBatches(t *testing.T) {
	base := time.Date(2026, 10, 23, 9, 0, 0, 0, time.UTC)
	seg := func(sec int, key string) *v1.CaptureSegment {
		g := &v1.CaptureSegment{Speaker: v1.Speaker_SPEAKER_WEARER, SpokenAt: timestamppb.New(base.Add(time.Duration(sec) * time.Second))}
		if key != "" {
			g.Speaker, g.SpeakerKey = v1.Speaker_SPEAKER_OTHER, key
		}
		return g
	}
	batches := captureBatches([]*v1.CaptureSegment{seg(0, "li"), seg(10, ""), seg(30, "wang"), seg(59, "li"), seg(60, "wang"), seg(200, "li")})
	var got []string
	for _, b := range batches {
		var keys []string
		for _, g := range b {
			keys = append(keys, g.GetSpeakerKey())
		}
		got = append(got, strings.Join(keys, ","))
	}
	if want := []string{"other-1,,other-2,other-1", "other-1", "other-1"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("batches = %q, want %q", got, want)
	}
}

// 碎片率与纯度按交谈的范围计算：交谈的主题是范围内最多的主题；关联几个项目的交谈对每个项目算作那个项目自己的主题。
func TestCheckGroupingByRange(t *testing.T) {
	topics := map[string]string{"s/2.0": "okr", "s/2.1": "okr", "s/2.2": "kangning", "s/3.0": "okr", "s/4.0": "kangning", "s/5.0": "", "s/6.0": "chen", "s/7.0": "kangning"}
	link := func(ps ...string) []xinlimock.Link {
		var out []xinlimock.Link
		for _, p := range ps {
			out = append(out, xinlimock.Link{Match: "existing", Project: p})
		}
		return out
	}
	st := &xinlimock.State{Exchanges: []xinlimock.Exchange{
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

// 仓库中的评测用例都能解析：用例的字段写错时在 make check 里就发现，而不是等到花钱的 make eval。
func TestShippedSuiteLoads(t *testing.T) {
	if _, err := LoadSuite("../../evals"); err != nil {
		t.Fatal(err)
	}
}
