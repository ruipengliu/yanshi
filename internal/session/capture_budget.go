package session

import (
	"strings"

	"yanshi/internal/model"
)

// MinCaptureToolResult 为完整一段 Capture 预留结果预算。放在 session 是为了让 AgentDef 的加载校验
// 不反向依赖 capture（capture 与 capability 都依赖 agentdef）。每块分别估算，保留向上取整的开销。
func MinCaptureToolResult() int {
	// 原话每字至多 4 个 UTF-8 字节；64 字节覆盖时刻、最长 speaker_key 与标注。
	// 其余依次为续页标题、日期头（含之后七天）、引用编号、最长页尾（含两个完整位置）。
	// 保守预留避免位置位数或日期变化使合法的最长片段无法读出；capture 测试核对这些上限。
	sizes := []int{128, 512, 64, 4*MaxCaptureText + 64, 256}
	total := 0
	for _, size := range sizes {
		total += model.EstimateTokens(model.TextBlocks(strings.Repeat("x", size)))
	}
	return total
}
