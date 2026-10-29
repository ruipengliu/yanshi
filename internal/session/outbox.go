package session

import (
	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/askuser"
	"yanshi/internal/outbox"
)

// SelectEnvelope 从事件应用前的合法投影选择信封；调用方须先校验整批事件。
// 不读索引，不分配随机 ID，也不改变投影，使重放、快照恢复与冲突重试得到同一结果。
func SelectEnvelope(before *State, e *v1.Event) *outbox.Envelope {
	var kind outbox.Kind
	var runID, callID string
	switch p := e.GetPayload().(type) {
	case *v1.Event_RunCompleted:
		kind, runID = outbox.RunCompleted, p.RunCompleted.GetRunId()
	case *v1.Event_RunFailed:
		kind, runID = outbox.RunFailed, p.RunFailed.GetRunId()
	case *v1.Event_ToolCallStarted:
		m := p.ToolCallStarted
		r := before.Run(m.GetRunId())
		if r == nil {
			return nil
		}
		c := r.call(m.GetCallId())
		if c == nil || c.Started() || c.Call.GetCapability() != askuser.Capability || m.GetNodeId() != askuser.NodeID {
			return nil
		}
		kind, runID, callID = outbox.Question, m.GetRunId(), m.GetCallId()
	case *v1.Event_ApprovalRequested:
		kind, runID, callID = outbox.ApprovalRequested, p.ApprovalRequested.GetRunId(), p.ApprovalRequested.GetCallId()
	case *v1.Event_RunRequested:
		if b := BackgroundInput(p.RunRequested.GetInput()); b != nil {
			kind, callID = outbox.BackgroundSettled, b.GetCallId()
		}
	case *v1.Event_Steered:
		if b := BackgroundInput(p.Steered.GetInput()); b != nil {
			kind, callID = outbox.BackgroundSettled, b.GetCallId()
		}
	case *v1.Event_BackgroundCancelled:
		kind, callID = outbox.BackgroundSettled, p.BackgroundCancelled.GetCallId()
	case *v1.Event_SessionClosed:
		kind = outbox.SessionClosed
	}
	if kind == "" || before.Created == nil {
		return nil
	}
	if kind == outbox.BackgroundSettled {
		if b := before.Background[callID]; b != nil {
			runID = b.RunID
		}
	}
	return &outbox.Envelope{
		EnvelopeID:   outbox.ID(before.SessionID, e.GetSeq(), kind),
		BusinessLine: before.Created.GetBusinessLine(), EndUser: before.Created.GetEndUser(),
		SessionID: before.SessionID, Seq: e.GetSeq(), Kind: kind, RunID: runID, CallID: callID,
		Agent: before.Agent.GetName(), AgentVersion: before.Agent.GetVersion(),
	}
}

func selectEnvelopes(st *State, events []*v1.Event) ([]outbox.Envelope, error) {
	before := st.Clone()
	var envelopes []outbox.Envelope
	for _, e := range events {
		if envelope := SelectEnvelope(before, e); envelope != nil {
			envelopes = append(envelopes, *envelope)
		}
		if err := before.Apply(e); err != nil {
			return nil, err
		}
	}
	return envelopes, nil
}
