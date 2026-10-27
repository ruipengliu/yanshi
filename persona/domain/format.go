package domain

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// Ref 实现线上引用的数字部分，见 docs/design/captures.md §5–§6。
// 引用格式属于公开协议，Persona 自己解析，不依赖内核实现。
type Ref struct {
	Seq   uint64
	Index int
}

func (r Ref) String() string { return fmt.Sprintf("%d.%d", r.Seq, r.Index) }
func ParseRef(s string) (Ref, bool) {
	seqText, indexText, hasDot := strings.Cut(s, ".")
	seq, seqErr := strconv.ParseUint(seqText, 10, 64)
	index, indexErr := strconv.Atoi(indexText)
	if !hasDot || seqErr != nil || indexErr != nil || seq == 0 || index < 0 {
		return Ref{}, false
	}
	return Ref{Seq: seq, Index: index}, true
}

// textSimilarity 实现 docs/design/xinli-model.md §12 的字符二元组 Dice 规则。
// 标点与空白断开词，单字符词保留；按集合计数，重复字符组不额外加权。
func textSimilarity(a, b string) float64 {
	grams := func(text string) map[string]bool {
		set := map[string]bool{}
		var word []rune
		flush := func() {
			if len(word) == 1 {
				set[string(word)] = true
			}
			for i := 1; i < len(word); i++ {
				set[string(word[i-1:i+1])] = true
			}
			word = word[:0]
		}
		for _, r := range strings.ToLower(text) {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				word = append(word, r)
			} else {
				flush()
			}
		}
		flush()
		return set
	}
	left, right := grams(a), grams(b)
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	common := 0
	for gram := range left {
		if right[gram] {
			common++
		}
	}
	return 2 * float64(common) / float64(len(left)+len(right))
}
