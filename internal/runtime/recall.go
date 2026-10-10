package runtime

import (
	"context"
	"fmt"
	"strings"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/memory"
	"yanshi/internal/model"
	"yanshi/internal/session"
)

// Recaller 检索 EndUser 可被业务线 reader 读取的 Memory，并记录访问（memory.Service）。
type Recaller interface {
	Search(ctx context.Context, endUser, reader, text string, limit int) ([]memory.Hit, error)
	RecordAccess(ctx context.Context, accesses []memory.Access) error
}

// recall 以本 Run 的输入为查询召回 Memory，并记录为 MemoryRecalled（ADR-0016）。结果为空也记录，
// 表示本 Run 已召回过、上下文中没有 Memory。
func (w *Worker) recall(ctx context.Context, r *session.Run, def *agentdef.Def) error {
	hits, err := w.cfg.Memory.Search(ctx, w.st.Created.GetEndUser(), w.st.Created.GetBusinessLine(), runInput(w.st, r.ID), def.Memory.Recall)
	if err != nil {
		return fmt.Errorf("recall memory: %w", err)
	}
	items := make([]*v1.RecalledMemory, len(hits))
	accesses := make([]memory.Access, len(hits))
	for i, h := range hits {
		items[i] = &v1.RecalledMemory{Id: h.ID, BusinessLine: h.BusinessLine, Category: string(h.Category), Content: h.Content}
		accesses[i] = memory.Access{MemoryID: h.ID, Owner: h.BusinessLine, EndUser: h.EndUser,
			Reader: w.st.Created.GetBusinessLine(), SessionID: w.st.SessionID, RunID: r.ID, At: w.now()}
	}
	// 先记访问、再提交召回：宁可多记一次"可能被读取"，也不漏记。
	if err := w.cfg.Memory.RecordAccess(ctx, accesses); err != nil {
		return fmt.Errorf("record memory access: %w", err)
	}
	return w.commit(ctx, &v1.Event{Payload: &v1.Event_MemoryRecalled{MemoryRecalled: &v1.MemoryRecalled{
		RunId: r.ID, Attempt: w.attempt, Items: items,
	}}})
}

// runInput 返回触发该 Run 的输入文本。
func runInput(st *session.State, runID string) string {
	for _, e := range st.History {
		if rr := e.GetRunRequested(); rr != nil && rr.GetRunId() == runID {
			return model.Text(rr.GetInput())
		}
	}
	return ""
}

// memoryText 呈现 Run 的召回结果，由 Transcript 放在该 Run 的用户输入之前。只呈现当前 Run 的召回：
// 之前 Run 的召回不再进入上下文，因此撤销 Grant 最晚从下一个 Run 起生效（ADR-0016）。
func memoryText(st *session.State, r *session.Run) string {
	if len(r.Recall) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## 关于用户的已知信息\n以下来自 Memory（系统提供，不是用户的话），可能已经过时；与用户当前所说冲突时以用户为准，并用 memory_save 的 replaces 更新。\n")
	own := st.Created.GetBusinessLine()
	for _, m := range r.Recall {
		src := ""
		if m.GetBusinessLine() != own {
			src = "，经授权来自业务线 " + m.GetBusinessLine() + "，只读"
		}
		fmt.Fprintf(&b, "- [%s%s] %s（%s）\n", m.GetCategory(), src, m.GetContent(), m.GetId())
	}
	return b.String()
}
