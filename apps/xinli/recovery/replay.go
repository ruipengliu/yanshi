package recovery

import (
	"encoding/json"
	"errors"
	"strconv"

	"xinli/kernel"
)

// replayEvent 只解码公开 Event 中选择信封所需字段，正文不会进入持久检查点。
type replayEvent struct {
	SessionID string      `json:"session_id"`
	Seq       json.Number `json:"seq"`
	Created   *struct {
		BusinessLine string `json:"business_line"`
		EndUser      string `json:"end_user"`
		Agent        Agent  `json:"agent"`
	} `json:"session_created"`
	Switched *struct {
		To Agent `json:"to"`
	} `json:"agent_switched"`
	Message *struct {
		RunID string `json:"run_id"`
		Calls []struct {
			ID         string `json:"call_id"`
			Capability string `json:"capability"`
		} `json:"tool_calls"`
	} `json:"assistant_message"`
	Started     *callEvent `json:"tool_call_started"`
	Result      *callEvent `json:"tool_result"`
	Completed   *callEvent `json:"run_completed"`
	Failed      *callEvent `json:"run_failed"`
	Interrupted *callEvent `json:"run_interrupted"`
	Approval    *callEvent `json:"approval_requested"`
	Requested   *callEvent `json:"run_requested"`
	Steered     *callEvent `json:"steered"`
	Cancelled   *callEvent `json:"background_cancelled"`
	Closed      *struct{}  `json:"session_closed"`
}

type callEvent struct {
	RunID        string    `json:"run_id"`
	CallID       string    `json:"call_id"`
	NodeID       string    `json:"node_id"`
	Backgrounded *struct{} `json:"backgrounded"`
	Input        []struct {
		Background *struct {
			CallID string `json:"call_id"`
		} `json:"background_result"`
	} `json:"input"`
}

// Replay 与公开出站选择规则保持一致。跨页保留首次派发和后台调用所属 Run，不能从当前投影猜历史。
// 调用方只在整页成功后提交；失败时丢弃 cp 的本地副本。
func Replay(line string, cp *Checkpoint, events []json.RawMessage) ([]kernel.Envelope, error) {
	if cp.Questions == nil {
		cp.Questions = map[string]QuestionCall{}
	}
	if cp.Background == nil {
		cp.Background = map[string]string{}
	}
	out := []kernel.Envelope{}
	for _, raw := range events {
		var e replayEvent
		if json.Unmarshal(raw, &e) != nil {
			return nil, errors.New("recovery: invalid event")
		}
		seq, err := strconv.ParseUint(string(e.Seq), 10, 64)
		if err != nil || seq != cp.Seq+1 || e.SessionID != cp.SessionID {
			return nil, errors.New("recovery: event gap or identity mismatch")
		}
		if cp.Seq == 0 && e.Created == nil {
			return nil, errors.New("recovery: missing creation event")
		}
		envelope := kernel.Envelope{BusinessLine: line, EndUser: cp.EndUser, SessionID: cp.SessionID,
			Seq: seq, Agent: cp.Agent.Name, AgentVersion: cp.Agent.Version}
		switch {
		case e.Completed != nil:
			envelope.Kind, envelope.RunID = "run_completed", e.Completed.RunID
		case e.Failed != nil:
			envelope.Kind, envelope.RunID = "run_failed", e.Failed.RunID
		case e.Approval != nil:
			envelope.Kind, envelope.RunID, envelope.CallID = "approval_requested", e.Approval.RunID, e.Approval.CallID
		case e.Started != nil:
			key := questionKey(e.Started.RunID, e.Started.CallID)
			if call, ok := cp.Questions[key]; ok {
				if !call.Started && e.Started.NodeID == "@user" {
					envelope.Kind, envelope.RunID, envelope.CallID = "question", e.Started.RunID, e.Started.CallID
				}
				call.Started = true
				cp.Questions[key] = call
			}
		case e.Closed != nil:
			envelope.Kind = "session_closed"
		}
		background := ""
		for _, input := range []*callEvent{e.Requested, e.Steered} {
			if input != nil && len(input.Input) == 1 && input.Input[0].Background != nil {
				background = input.Input[0].Background.CallID
			}
		}
		if e.Cancelled != nil {
			background = e.Cancelled.CallID
		}
		if background != "" {
			envelope.Kind, envelope.CallID, envelope.RunID = "background_settled", background, cp.Background[background]
			delete(cp.Background, background)
		}
		if envelope.Kind != "" {
			out = append(out, envelope)
		}
		if e.Created != nil {
			if cp.Seq != 0 || e.Created.BusinessLine != line || e.Created.EndUser != cp.EndUser {
				return nil, errors.New("recovery: creation owner mismatch")
			}
			cp.Agent = e.Created.Agent
		}
		if e.Switched != nil {
			cp.Agent = e.Switched.To
		}
		if e.Message != nil {
			for _, call := range e.Message.Calls {
				if call.Capability == "ask_user" {
					cp.Questions[questionKey(e.Message.RunID, call.ID)] = QuestionCall{RunID: e.Message.RunID}
				}
			}
		}
		if e.Result != nil {
			delete(cp.Questions, questionKey(e.Result.RunID, e.Result.CallID))
			if e.Result.Backgrounded != nil {
				cp.Background[e.Result.CallID] = e.Result.RunID
			}
		}
		for _, terminal := range []*callEvent{e.Completed, e.Failed, e.Interrupted} {
			if terminal != nil {
				for key, call := range cp.Questions {
					if call.RunID == terminal.RunID {
						delete(cp.Questions, key)
					}
				}
			}
		}
		cp.Seq = seq
	}
	return out, nil
}

func questionKey(run, call string) string {
	return strconv.Itoa(len(run)) + ":" + run + call
}
