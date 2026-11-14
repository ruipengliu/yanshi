package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// LowSupportSpan 保留原始文字的 rune 区间，不能用规范化后的文字或字节偏移解释。
type LowSupportSpan struct {
	Start         int      `json:"start"`
	End           int      `json:"end"`
	CriticalKinds []string `json:"critical_kinds,omitempty"`
}

func (g CaptureSegment) EffectiveSpeakerGrade() string {
	if g.Speaker == "wearer" && g.SpeakerGrade == "" {
		return "evidence"
	}
	return g.SpeakerGrade
}

func (g CaptureSegment) EffectiveDisposition() string {
	if g.Disposition == "" {
		return "accept"
	}
	return g.Disposition
}

func (s LowSupportSpan) EffectiveCriticalKinds() []string {
	if len(s.CriticalKinds) == 0 {
		return []string{"unknown"}
	}
	return slices.Clone(s.CriticalKinds)
}

// ValidateMetadata 只校验业务侧元数据，不回填缺省值，以保留旧 JSON 与去重摘要。
func (g CaptureSegment) ValidateMetadata() error {
	if g.SpeakerGrade != "" && (g.Speaker != "wearer" || g.SpeakerGrade != "evidence" && g.SpeakerGrade != "timeline") {
		return fmt.Errorf("invalid capture speaker_grade")
	}
	switch g.EffectiveDisposition() {
	case "accept":
		if g.DispositionReason != "" {
			return fmt.Errorf("invalid capture disposition_reason")
		}
	case "quarantine":
		switch g.DispositionReason {
		case "playback", "suspected_replay", "hallucination_disagreement":
		default:
			return fmt.Errorf("invalid capture disposition_reason")
		}
	default:
		return fmt.Errorf("invalid capture disposition")
	}
	if g.Background && g.Speaker == "wearer" {
		return fmt.Errorf("invalid capture background")
	}
	end, count := 0, utf8.RuneCountInString(g.Text)
	for _, span := range g.LowSupportSpans {
		if span.Start < end || span.Start >= span.End || span.End > count {
			return fmt.Errorf("invalid capture low_support_spans")
		}
		end = span.End
		seen := map[string]bool{}
		for _, kind := range span.CriticalKinds {
			switch kind {
			case "person", "date", "number", "negation", "modality", "correction", "none", "unknown":
			default:
				return fmt.Errorf("invalid capture critical_kinds")
			}
			if seen[kind] || kind == "none" && len(span.CriticalKinds) != 1 {
				return fmt.Errorf("invalid capture critical_kinds")
			}
			seen[kind] = true
		}
	}
	return nil
}

func (g CaptureSegment) Clone() CaptureSegment {
	g.LowSupportSpans = slices.Clone(g.LowSupportSpans)
	for i := range g.LowSupportSpans {
		g.LowSupportSpans[i].CriticalKinds = slices.Clone(g.LowSupportSpans[i].CriticalKinds)
	}
	return g
}

func CloneCaptureSegments(segments []CaptureSegment) []CaptureSegment {
	out := slices.Clone(segments)
	for i := range out {
		out[i] = out[i].Clone()
	}
	return out
}

// Equal 比较保存的值而非解释后的缺省值；显式元数据仍属于批次身份。
func (g CaptureSegment) Equal(other CaptureSegment) bool {
	return g.Speaker == other.Speaker && g.SpeakerKey == other.SpeakerKey && g.SpeakerRef == other.SpeakerRef &&
		g.Text == other.Text && g.SpokenAt == other.SpokenAt && g.TimeZone == other.TimeZone &&
		g.SpeakerGrade == other.SpeakerGrade && g.Disposition == other.Disposition &&
		g.DispositionReason == other.DispositionReason && g.Background == other.Background &&
		slices.EqualFunc(g.LowSupportSpans, other.LowSupportSpans, func(a, b LowSupportSpan) bool {
			return a.Start == b.Start && a.End == b.End && slices.Equal(a.CriticalKinds, b.CriticalKinds)
		})
}

// ValidateLegacyCaptureSegments 在第③步开放入口时移除；第①、②步只在领域与存储接缝接受新状态。
func ValidateLegacyCaptureSegments(segments []CaptureSegment) error {
	for _, g := range segments {
		if g.Speaker != "wearer" && g.Speaker != "other" {
			return fmt.Errorf("invalid capture speaker")
		}
		if g.SpeakerGrade != "" || g.Disposition != "" ||
			g.DispositionReason != "" || g.Background || g.LowSupportSpans != nil {
			return ErrCaptureInvalid
		}
	}
	return nil
}

// DecodeLegacyCaptureSegment 还检查字段是否出现，避免 false、空值或 null 绕过临时旧契约。
// out 使用入口自己的旧字段解码规则；此限制同样在第③步移除。
func DecodeLegacyCaptureSegment(data []byte, out any) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for name := range fields {
		for _, field := range []string{"speaker_grade", "disposition", "disposition_reason", "background", "low_support_spans"} {
			if strings.EqualFold(name, field) {
				return ErrCaptureInvalid
			}
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(out)
}

// LegacyCaptureSegments 只用于写入口，存储解码仍直接使用 CaptureSegment。
type LegacyCaptureSegments []CaptureSegment

func (s *LegacyCaptureSegments) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var out LegacyCaptureSegments
	if raw != nil {
		out = make(LegacyCaptureSegments, len(raw))
	}
	for i := range raw {
		if err := DecodeLegacyCaptureSegment(raw[i], &out[i]); err != nil {
			return err
		}
	}
	*s = out
	return nil
}
