package session

import (
	"fmt"
	"maps"
	"slices"

	v1 "yanshi/gen/yanshi/v1"
)

// StreamTranscript 是旁听转写的流（ADR-0036、ADR-0039）：佩戴者与他人的日常对话，按说话人区分来源。
const StreamTranscript = "transcript"

// streams 是平台登记的流的种类（docs/design/m5-inputs-triggers-loops.md §5）。种类决定片段的校验（CheckCapture）、
// 内容安全的送检方式、来源的推导（Provenance）与给模型看的格式；业务线只能在 AgentDef 中声明接收哪些已登记的流。
// 非文字的种类每一种另立 ADR（ADR-0036 只收文字）。
var streams = map[string]bool{StreamTranscript: true}

// KnownStream 报告 name 是否是平台登记的流。
func KnownStream(name string) bool { return streams[name] }

// checkCaptureRead 校验 captures 结果中的读取位置。位置本身由 captures 解析，这里只校验结构。
func checkCaptureRead(rd *v1.CaptureRead) error {
	switch {
	case !KnownStream(rd.GetStream()):
		return fmt.Errorf("unknown stream %q", rd.GetStream())
	case rd.GetAfter() == "":
		return fmt.Errorf("empty position")
	case rd.GetExchangeGapMs() <= 0:
		return fmt.Errorf("exchange gap %dms", rd.GetExchangeGapMs())
	}
	// 旧的未要求确认的结果仍可只有 after；新页必须同时提供起点与首段，防止拼接伪造的断链。
	if rd.GetAck() || rd.GetStart() != "" || rd.GetFirst() != "" {
		start, ok1 := capturePosition(rd.GetStart())
		first, ok2 := parseCaptureRef(rd.GetFirst())
		last, ok3 := capturePosition(rd.GetAfter())
		if rd.GetStart() == "" || !ok1 || !ok2 || !ok3 || !start.before(first) || last.before(first) {
			return fmt.Errorf("invalid capture page range")
		}
	}
	return nil
}

// sortedReads 按流名排序，快照的序列化因此是确定的。
func sortedReads(m map[string]*v1.CaptureRead) []*v1.CaptureRead {
	out := make([]*v1.CaptureRead, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		out = append(out, m[k])
	}
	return out
}

func readMap(rs []*v1.CaptureRead) map[string]*v1.CaptureRead {
	if len(rs) == 0 {
		return nil
	}
	m := make(map[string]*v1.CaptureRead, len(rs))
	for _, r := range rs {
		m[r.GetStream()] = r
	}
	return m
}
