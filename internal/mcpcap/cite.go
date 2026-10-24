package mcpcap

import (
	"context"
	"encoding/json"
	"maps"
	"regexp"
	"slices"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/capture"
	"yanshi/internal/eventlog"
)

// Citation 是平台对一个旁听引用的解析（docs/design/captures-by-exchange.md §7）：被引用片段的说话人。不含文字：他人的
// 原话只保留在旁听 Session 中（ADR-0037），不经这条路径流到业务线长期保存。
type Citation struct {
	Ref        string `json:"ref"`
	Speaker    string `json:"speaker,omitempty"` // "wearer" 或 "other"
	SpeakerKey string `json:"speaker_key,omitempty"`
	Redacted   bool   `json:"redacted,omitempty"`
	// Missing 表示本 Session 中没有这段旁听：编号写错，或引用了别的 Session。
	Missing bool `json:"missing,omitempty"`
}

// MaxCitations 是一次调用至多解析的引用数，超出的忽略：每个引用读一次日志。
const MaxCitations = 200

var refRE = regexp.MustCompile(`^#([0-9]+\.[0-9]+)$`)

// Citations 找出参数中恰为引用编号的字符串（如 "#57.3"），按 sessionID 的日志解析，按首次出现的顺序返回（对象按键名的
// 顺序遍历），同一编号只解析一次。结果只取决于日志，而日志只追加，所以重放时得到同样的结果。
func Citations(ctx context.Context, log eventlog.Log, sessionID string, args json.RawMessage) ([]Citation, error) {
	var v any
	if err := json.Unmarshal(args, &v); err != nil {
		return nil, nil // 参数不是 JSON 时没有引用；工具自己报告参数错误
	}
	var refs []string
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
	out := make([]Citation, 0, len(refs))
	for _, r := range refs {
		c := Citation{Ref: "#" + r, Missing: true}
		ref, ok := capture.ParseRef(r)
		if ok {
			events, err := log.Read(ctx, sessionID, ref.Seq-1, 1)
			if err != nil {
				return nil, err
			}
			if len(events) == 1 && events[0].GetSeq() == ref.Seq {
				if segs := events[0].GetCaptureAppended().GetSegments(); ref.Index < len(segs) {
					g := segs[ref.Index]
					c = Citation{Ref: c.Ref, Speaker: "wearer", Redacted: g.GetRedacted()}
					if g.GetSpeaker() == v1.Speaker_SPEAKER_OTHER {
						c.Speaker, c.SpeakerKey = "other", g.GetSpeakerKey()
					}
				}
			}
		}
		out = append(out, c)
	}
	return out, nil
}
