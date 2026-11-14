package devui

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"persona/domain"
)

// 与 xinli-live-brief 的 input.max_chars 对齐，标签与换行也占字符预算。
const briefMaxChars = 8000
const briefMaxSegments = 200

type briefRevision struct {
	date     string
	revision uint64
	deleted  bool
}

func captureRevision(b *domain.Book, date string) briefRevision {
	day := b.BusinessDay(date)
	return briefRevision{date: date, revision: day.CaptureRevision, deleted: day.Deleted}
}

func briefCaptureInput(segments []domain.CaptureView) string {
	visible := make([]domain.PageSegment, 0, len(segments))
	for _, s := range segments {
		ref, ok := domain.ParseRef(s.Ref)
		if !ok {
			continue
		}
		if p, ok := domain.ProjectCapture(ref, s.CaptureSegment); ok {
			visible = append(visible, p)
		}
	}
	return briefInput(visible)
}

func briefInput(segments []domain.PageSegment) string {
	var lines []string
	remaining := briefMaxChars
	for i := len(segments) - 1; i >= max(0, len(segments)-briefMaxSegments); i-- {
		s := segments[i]
		label := "[佩戴者] "
		if s.Speaker == "other" {
			label = "[他人 " + s.SpeakerKey
			if s.SpeakerRef != "" {
				label += " @" + s.SpeakerRef
			}
			label += "] "
		}
		// 原文中的换行不能冒充另一段带标签的发言。
		text := strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return ' '
			}
			return r
		}, s.Text)
		budget := remaining - utf8.RuneCountInString(label)
		if len(lines) > 0 {
			budget--
		}
		if budget <= 0 {
			break
		}
		runes := []rune(text)
		cut := len(runes) > budget
		if cut {
			runes = runes[len(runes)-budget:]
		}
		line := label + string(runes)
		remaining -= utf8.RuneCountInString(line)
		if len(lines) > 0 {
			remaining--
		}
		lines = append(lines, line)
		if cut {
			break
		}
	}
	slices.Reverse(lines)
	return strings.Join(lines, "\n")
}
