package session

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	v1 "yanshi/gen/yanshi/v1"
)

// captureRef 只比较日志位置，不用相邻序号推断旁听是否连续：两批旁听之间可能夹着其他 Event。
type captureRef struct {
	seq   uint64
	index int
}

func parseCaptureRef(s string) (captureRef, bool) {
	a, b, ok := strings.Cut(s, ".")
	seq, e1 := strconv.ParseUint(a, 10, 64)
	index, e2 := strconv.Atoi(b)
	r := captureRef{seq: seq, index: index}
	return r, ok && e1 == nil && e2 == nil && seq > 0 && index >= 0 && s == fmt.Sprintf("%d.%d", seq, index)
}

func (a captureRef) before(b captureRef) bool {
	return a.seq < b.seq || (a.seq == b.seq && a.index < b.index)
}

func capturePosition(s string) (captureRef, bool) {
	if s == "" || s == "0" {
		return captureRef{}, true
	}
	last, span, cont := strings.Cut(s, "~")
	r, ok := parseCaptureRef(last)
	if cont {
		start, valid := parseCaptureRef(span)
		ok = ok && valid && !r.before(start)
	}
	return r, ok
}

// CheckCaptureAck 校验业务线的回执结构；格式错误时 Worker 丢弃元数据，Apply 则拒绝写入。
func CheckCaptureAck(ack *v1.CaptureAck) error {
	from, ok1 := parseCaptureRef(ack.GetFrom())
	through, ok2 := parseCaptureRef(ack.GetThrough())
	if !KnownStream(ack.GetStream()) || !ok1 || !ok2 || through.before(from) {
		return fmt.Errorf("invalid capture ack")
	}
	return nil
}

// mcpCallName 匹配调用日志中的模型工具名；mcp: 是 AgentDef 白名单的写法，不写入 ToolCall。
var mcpCallName = regexp.MustCompile(`^mcp_[a-z0-9][a-z0-9-]{0,23}__[a-zA-Z0-9_-]+$`)

func isMCPCall(name string) bool { return mcpCallName.MatchString(name) }

// confirmedCaptureRead 只沿本 Run 实际读过的页拼链：Ref 的序号间隙不能证明两批旁听相接（ADR-0049）。
func confirmedCaptureRead(committed *v1.CaptureRead, stream string, pages []*v1.CaptureRead, acks []*v1.CaptureAck) *v1.CaptureRead {
	cur := committed.GetAfter()
	if cur == "" {
		cur = "0"
	}
	var result *v1.CaptureRead
	for {
		var best *v1.CaptureRead
		var farthest captureRef
		for _, page := range pages {
			if page.GetStream() != stream || page.GetStart() != cur {
				continue
			}
			first, _ := parseCaptureRef(page.GetFirst())
			last, _ := capturePosition(page.GetAfter())
			if best != nil && !farthest.before(last) {
				continue
			}
			for _, ack := range acks {
				from, _ := parseCaptureRef(ack.GetFrom())
				through, _ := parseCaptureRef(ack.GetThrough())
				if ack.GetStream() == stream && !first.before(from) && !through.before(last) {
					best, farthest = page, last
					break
				}
			}
		}
		if best == nil {
			return result
		}
		result, cur = best, best.GetAfter()
	}
}
