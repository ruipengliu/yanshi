package session

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	v1 "yanshi/gen/yanshi/v1"
)

// MaxCaptureText 限制可信页中每段的字符数。
const MaxCaptureText = 2000

// speakerKeyRE 限制他人的说话人键：它原样出现在给模型看的文本中（"[他人 other-1]"），不能夹带别的内容。
var speakerKeyRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

var speakerRefRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

var attestedRefRE = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)

// ValidSpeakerKey 与可信页共用说话人键的边界，避免标注夹带文字。
func ValidSpeakerKey(key string) bool { return speakerKeyRE.MatchString(key) }

// ValidSpeakerRef 只接受他人的可选不透明引用，不据此推导来源或校验业务实体。
func ValidSpeakerRef(speaker v1.Speaker, ref string) bool {
	return ref == "" || speaker == v1.Speaker_SPEAKER_OTHER && speakerRefRE.MatchString(ref)
}

// validateCaptureSegment 校验 attested 页中的每个片段：attested 标记只证明来源，不能绕过片段本身的边界。
func validateCaptureSegment(g *v1.CaptureSegment) error {
	switch g.GetSpeaker() {
	case v1.Speaker_SPEAKER_WEARER:
		if g.GetSpeakerKey() != "" {
			return fmt.Errorf("wearer with a speaker key in capture result")
		}
	case v1.Speaker_SPEAKER_OTHER:
		if !ValidSpeakerKey(g.GetSpeakerKey()) {
			return fmt.Errorf("invalid speaker key in capture result")
		}
	default:
		return fmt.Errorf("invalid speaker in capture result")
	}
	if !ValidSpeakerRef(g.GetSpeaker(), g.GetSpeakerRef()) {
		return fmt.Errorf("invalid speaker ref in capture result")
	}
	if g.GetRedacted() != (g.GetText() == "") || utf8.RuneCountInString(g.GetText()) > MaxCaptureText {
		return fmt.Errorf("invalid text in capture result")
	}
	return nil
}

// ValidAttestedRef 只校验业务线引用的形状，不把它解释为日志位置。
func ValidAttestedRef(ref string) bool {
	a, b, ok := strings.Cut(ref, ".")
	_, e1 := strconv.ParseUint(a, 10, 64)
	n, e2 := strconv.Atoi(b)
	return ok && e1 == nil && e2 == nil && n >= 0 && attestedRefRE.MatchString(ref)
}

func hasCapture(blocks []*v1.ContentBlock) bool {
	return slices.ContainsFunc(blocks, func(b *v1.ContentBlock) bool { return b.GetCapture() != nil })
}
