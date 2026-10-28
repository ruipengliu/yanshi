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
	date      string
	lastRef   string
	deletions int
	deleted   bool
}

func captureRevision(b *domain.Book, date string, segments []domain.PageSegment) briefRevision {
	r := briefRevision{date: date, deleted: b.BusinessDay(date).Deleted}
	if len(segments) > 0 {
		r.lastRef = segments[len(segments)-1].Ref
	}
	// 批次引用永不复用，删除待办也不随完成而移除：组合可识别追加和任意范围擦除，
	// 不把工作 Session 换绑或整理进度变化误当作原文变化。
	for _, task := range b.DeletionTasks() {
		if task.Date == date {
			r.deletions++
		}
	}
	return r
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
