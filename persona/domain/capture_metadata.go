package domain

import (
	"fmt"
	"slices"
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
	if g.Speaker != "wearer" && g.Speaker != "other" && g.Speaker != "unknown" {
		return fmt.Errorf("invalid capture speaker")
	}
	if g.Speaker == "unknown" {
		if g.SpeakerKey != "" {
			return fmt.Errorf("invalid capture speaker_key")
		}
		if g.SpeakerRef != "" {
			return fmt.Errorf("invalid capture speaker_ref")
		}
	}
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
