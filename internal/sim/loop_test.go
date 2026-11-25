package sim

import "testing"

// TestExternalLoopWithFaults：由外部循环驱动的 Run 在 Worker 崩溃、存储故障、接管、压缩与内容安全的等待和拦截之下，
// 叠加循环自己的超时、5xx、不合法的回答、回答后写入前的崩溃与旧 Attempt 的迟到回答（ADR-0062"必须验证"）。
// 每一步请求对照日志检查（checkLoopRequest），日志另由 checkLoopLog 检查；删除不变量照常检查日志与快照，
// 状态只存在于这两处。
func TestExternalLoopWithFaults(t *testing.T) {
	t.Parallel()
	total := parallelSeeds(t, seedList(), func(t *testing.T, s uint64) Stats {
		return run(t, s, Options{Faults: true, ExternalLoop: true}).Stats
	})
	requireCoverage(t, total, "LoopSteps", "LoopRepeats", "LoopStreamed", "LoopTimeouts", "LoopServerErrors", "LoopInvalidReplies",
		"LoopCrashesAfterAnswer", "LoopLateReplies", "Crashes", "Takeovers", "StoreFaults", "Compactions", "SummaryFaults",
		"AttemptsAfterCompaction", "OutputBlocks", "ModerationWaits", "ModerationResumes", "Deletions", "DeletionsCompleted",
		"Delegations", "Approvals", "Questions")
}

// 迟到回答的注入在一步之中步进其余 Worker，并等 Worker 的监视 goroutine 取消这一步：结果仍须与调度无关。
func TestExternalLoopIsDeterministic(t *testing.T) {
	checkSimulationDeterminism(t, func(s uint64) Options {
		return Options{Faults: true, ExternalLoop: true, LongRuns: s == 7}
	})
}
