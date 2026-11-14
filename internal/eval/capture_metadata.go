package eval

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// CaptureLowSupportSpan 在加载时把便于标注的子串换成原始文字的 rune 半开区间。
type CaptureLowSupportSpan struct {
	Start         int      `yaml:"start"`
	End           int      `yaml:"end"`
	CriticalKinds []string `yaml:"critical_kinds"`
	text          *string
}

func captureYAMLKeys(node *yaml.Node, allowed ...string) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: capture metadata must be a mapping", node.Line)
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i]
		found := false
		for _, name := range allowed {
			if key.Value == name {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("line %d: unknown capture field %q", key.Line, key.Value)
		}
	}
	return nil
}

func (s *CaptureLowSupportSpan) UnmarshalYAML(node *yaml.Node) error {
	if err := captureYAMLKeys(node, "text", "start", "end", "critical_kinds"); err != nil {
		return err
	}
	var value struct {
		Text          *string  `yaml:"text"`
		Start         *int     `yaml:"start"`
		End           *int     `yaml:"end"`
		CriticalKinds []string `yaml:"critical_kinds"`
	}
	if err := node.Decode(&value); err != nil {
		return err
	}
	hasText, hasOffset := false, false
	for i := 0; i+1 < len(node.Content); i += 2 {
		switch node.Content[i].Value {
		case "text":
			hasText = true
		case "start", "end":
			hasOffset = true
		}
	}
	if hasText && hasOffset {
		return fmt.Errorf("line %d: low_support_spans text and start/end are mutually exclusive", node.Line)
	}
	*s = CaptureLowSupportSpan{CriticalKinds: value.CriticalKinds}
	if hasText {
		if value.Text == nil || *value.Text == "" {
			return fmt.Errorf("line %d: low_support_spans text must be nonempty", node.Line)
		}
		s.text = value.Text
	} else {
		if value.Start == nil || value.End == nil {
			return fmt.Errorf("line %d: low_support_spans needs text or both start and end", node.Line)
		}
		s.Start, s.End = *value.Start, *value.End
	}
	return nil
}

func (g *CaptureSegment) UnmarshalYAML(node *yaml.Node) error {
	if err := captureYAMLKeys(node, "speaker", "key", "speaker_ref", "text", "at", "topic", "gold", "asr", "heard", "heard_key",
		"speaker_grade", "disposition", "disposition_reason", "background", "low_support_spans"); err != nil {
		return err
	}
	type plain CaptureSegment
	var value plain
	if err := node.Decode(&value); err != nil {
		return err
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == "low_support_spans" && value.ASR != "" && value.ASR != value.Text {
			return fmt.Errorf("line %d: capture asr changes text with low_support_spans", node.Line)
		}
	}
	for i := range value.LowSupportSpans {
		span := &value.LowSupportSpans[i]
		if span.text == nil {
			continue
		}
		at := strings.Index(value.Text, *span.text)
		// 从首个匹配的下一个 rune 起找，连重叠的多匹配也拒绝。
		if at < 0 {
			return fmt.Errorf("line %d: low_support_spans text %q has no match", node.Line, *span.text)
		}
		_, width := utf8.DecodeRuneInString(value.Text[at:])
		if strings.Contains(value.Text[at+width:], *span.text) {
			return fmt.Errorf("line %d: low_support_spans text %q must match uniquely", node.Line, *span.text)
		}
		span.Start = utf8.RuneCountInString(value.Text[:at])
		span.End = span.Start + utf8.RuneCountInString(*span.text)
		span.text = nil
	}
	*g = CaptureSegment(value)
	return nil
}

var captureSpeakerRefRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func validateCapture(g CaptureSegment) error {
	if g.Heard != "" && g.Heard != "wearer" && g.Heard != "other" && g.Heard != "unknown" {
		return fmt.Errorf("capture heard %q must be wearer, other or unknown", g.Heard)
	}
	if g.ASR != "" && g.ASR != g.Text && g.LowSupportSpans != nil {
		return fmt.Errorf("capture asr changes text with low_support_spans")
	}
	if err := validateCaptureMetadata(g); err != nil {
		return fmt.Errorf("clean: %w", err)
	}
	if err := validateCaptureMetadata(g.heard()); err != nil {
		return fmt.Errorf("noisy: %w", err)
	}
	return nil
}

// 与 Persona 的片段及元数据校验保持一致，但不反向依赖业务模块，也不回填有效缺省值。
func validateCaptureMetadata(g CaptureSegment) error {
	if g.Speaker != "wearer" && g.Speaker != "other" && g.Speaker != "unknown" {
		return fmt.Errorf("capture speaker %q must be wearer, other or unknown", g.Speaker)
	}
	if g.Speaker == "unknown" && (g.Key != "" || g.SpeakerRef != "") {
		return fmt.Errorf("unknown capture cannot have key or speaker_ref")
	}
	if g.Speaker == "wearer" && (g.Key != "" || g.SpeakerRef != "") {
		return fmt.Errorf("wearer capture cannot have key or speaker_ref")
	}
	if g.SpeakerRef != "" && !captureSpeakerRefRE.MatchString(g.SpeakerRef) {
		return fmt.Errorf("invalid capture speaker_ref")
	}
	if count := utf8.RuneCountInString(g.Text); count == 0 || count > 2000 {
		return fmt.Errorf("capture text must contain 1 to 2000 characters")
	}
	if g.SpeakerGrade != "" && (g.Speaker != "wearer" || g.SpeakerGrade != "evidence" && g.SpeakerGrade != "timeline") {
		return fmt.Errorf("invalid capture speaker_grade")
	}
	switch g.Disposition {
	case "", "accept":
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
			return fmt.Errorf("invalid capture low_support_spans boundaries or order")
		}
		end = span.End
		seen := map[string]bool{}
		for _, kind := range span.CriticalKinds {
			switch kind {
			case "person", "date", "number", "negation", "modality", "correction", "none", "unknown":
			default:
				return fmt.Errorf("invalid capture critical_kinds %q", kind)
			}
			if seen[kind] || kind == "none" && len(span.CriticalKinds) != 1 {
				return fmt.Errorf("invalid capture critical_kinds")
			}
			seen[kind] = true
		}
	}
	return nil
}
