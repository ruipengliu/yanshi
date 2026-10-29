package memory_test

import (
	"context"
	"errors"
	"testing"

	"yanshi/internal/clock"
	"yanshi/internal/memory"
)

type mismatchedMemory struct {
	memory.Store
	row *memory.Memory
}

func (s mismatchedMemory) Get(context.Context, string, string) (*memory.Memory, error) {
	return s.row, nil
}

func TestSaveChecksOwnershipEvenOnIdempotentHit(t *testing.T) {
	ctx := context.Background()
	r := memory.SaveRequest{BusinessLine: "a", EndUser: "u", SessionID: "s", CallID: "external/call", Category: memory.Preference, Content: "喜欢骑车"}
	for _, change := range []func(*memory.Memory){
		func(m *memory.Memory) { m.EndUser = "another-user" },
		func(m *memory.Memory) { m.BusinessLine = "another-line" },
		func(m *memory.Memory) { m.SourceSession = "another-session" },
		func(m *memory.Memory) { m.SourceCall = "another-call" },
	} {
		row := &memory.Memory{ID: "mem_" + r.CallID, EndUser: r.EndUser, BusinessLine: r.BusinessLine, SourceSession: r.SessionID, SourceCall: r.CallID}
		change(row)
		s := &memory.Service{Store: mismatchedMemory{Store: memory.NewMemStore(), row: row}, Clock: clock.Real{}}
		if got, err := s.Save(ctx, r); !errors.Is(err, memory.ErrRejected) || got != nil {
			t.Fatalf("idempotent hit returned another invocation: %+v, %v", got, err)
		}
	}
}

func TestCallMemoryIDPreservesFieldBoundariesAndBytes(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range []memory.SaveRequest{
		{EndUser: "u", BusinessLine: "a:b", SessionID: "c", CallID: "same"},
		{EndUser: "u", BusinessLine: "a", SessionID: "b:c", CallID: "same"},
		{EndUser: "u", BusinessLine: "a", SessionID: "s", CallID: "\xff"},
		{EndUser: "u", BusinessLine: "a", SessionID: "s", CallID: "\xfe"},
	} {
		id, err := memory.CallMemoryID(r)
		if err != nil || seen[id] {
			t.Fatalf("distinct identity collapsed: %s, %v", id, err)
		}
		seen[id] = true
		if again, err := memory.CallMemoryID(r); err != nil || again != id {
			t.Fatalf("ID is not deterministic: %s, %v", again, err)
		}
	}
}
