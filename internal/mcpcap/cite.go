package mcpcap

import (
	"context"
	"encoding/json"
	"maps"
	"regexp"
	"slices"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/capture"
	"yanshi/internal/eventlog"
)

// Citation 是平台对一个旁听引用的解析（docs/design/captures.md §6）：被引用片段的说话人与说话时间。不含文字：他人的
// 原话只保留在旁听 Session 中（ADR-0037），不经这条路径流到业务线长期保存。说话时间不是对话内容，业务线据此按说话
// 那天换算"明天""下周二"这类期限（docs/design/xinli-model.md §7）。
type Citation struct {
	Ref        string `json:"ref"`
	Speaker    string `json:"speaker,omitempty"` // "wearer" 或 "other"
	SpeakerKey string `json:"speaker_key,omitempty"`
	Redacted   bool   `json:"redacted,omitempty"`
	// SpokenAt 与 TimeZone 取自片段（HostApp 填写），没有时为空。
	SpokenAt time.Time `json:"spoken_at,omitzero"`
	TimeZone string    `json:"time_zone,omitempty"`
	// Missing 表示本 Session 中没有这段旁听：编号写错，或引用了别的 Session。
	Missing bool `json:"missing,omitempty"`
}

// MaxCitations 是一次调用至多解析的引用数，超出的忽略，限制业务线收到的元数据。
const MaxCitations = 200

var refRE = regexp.MustCompile(`^#([0-9]+\.[0-9]+)$`)

// citationRefs 按首次出现的顺序提取引用，对象按键名遍历，同一编号只解析一次。
func citationRefs(args json.RawMessage) []string {
	var v any
	if err := json.Unmarshal(args, &v); err != nil {
		return nil // 参数不是 JSON 时没有引用；工具自己报告参数错误
	}
	refs := make([]string, 0)
	seen := map[string]bool{}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			if m := refRE.FindStringSubmatch(x); m != nil && !seen[m[1]] && len(refs) < MaxCitations {
				seen[m[1]] = true
				refs = append(refs, m[1])
			}
		case []any:
			for _, y := range x {
				walk(y)
			}
		case map[string]any:
			for _, k := range slices.Sorted(maps.Keys(x)) {
				walk(x[k])
			}
		}
	}
	walk(v)
	return refs
}

// Citations 按日志位置逐个读取内置 captures 引用，不因 Session 中有 attested 结果而切换命名空间。
func Citations(ctx context.Context, log eventlog.Log, sessionID string, args json.RawMessage) ([]Citation, error) {
	refs := citationRefs(args)
	if refs == nil {
		return nil, nil
	}
	out := make([]Citation, 0, len(refs))
	for _, r := range refs {
		c := Citation{Ref: "#" + r, Missing: true}
		ref, ok := capture.ParseRef(r)
		if ok && ref.Seq > 0 {
			events, err := log.Read(ctx, sessionID, ref.Seq-1, 1)
			if err != nil {
				return nil, err
			}
			if len(events) == 1 && events[0].GetSeq() == ref.Seq {
				if segs := events[0].GetCaptureAppended().GetSegments(); ref.Index < len(segs) {
					g := segs[ref.Index]
					c = citation(c.Ref, g)
				}
			}
		}
		out = append(out, c)
	}
	return out, nil
}

// AttestedCitations 只解析业务线引用，查不到不回退到日志位置。through 为调用投影的序号，0 表示不限；
// History 会被压缩裁剪，因此从日志前缀中取同编号最近一次出现的片段。
func AttestedCitations(ctx context.Context, log eventlog.Log, sessionID string, through uint64, args json.RawMessage) ([]Citation, error) {
	refs := citationRefs(args)
	if refs == nil {
		return nil, nil
	}
	latest := map[string]*v1.CaptureSegment{}
	if len(refs) > 0 {
		events, err := eventlog.ReadAll(ctx, log, sessionID, 0)
		if err != nil {
			return nil, err
		}
		for _, e := range events {
			if through != 0 && e.GetSeq() > through {
				break
			}
			if r := e.GetToolResult(); r.GetAttested() {
				for _, b := range r.GetContent() {
					if g := b.GetCapture(); g != nil {
						latest[g.GetRef()] = g
					}
				}
			}
		}
	}
	out := make([]Citation, 0, len(refs))
	for _, r := range refs {
		c := Citation{Ref: "#" + r, Missing: true}
		if ref, ok := capture.ParseRef(r); ok {
			if g := latest[ref.String()]; g != nil {
				c = citation(c.Ref, g)
			}
		}
		out = append(out, c)
	}
	return out, nil
}

func citation(ref string, g *v1.CaptureSegment) Citation {
	c := Citation{Ref: ref, Speaker: "wearer", Redacted: g.GetRedacted(), TimeZone: g.GetTimeZone()}
	if g.GetSpokenAt() != nil {
		c.SpokenAt = g.GetSpokenAt().AsTime()
	}
	if g.GetSpeaker() == v1.Speaker_SPEAKER_OTHER {
		c.Speaker, c.SpeakerKey = "other", g.GetSpeakerKey()
	}
	return c
}
