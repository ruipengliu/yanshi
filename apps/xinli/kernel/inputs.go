package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
)

// InputReceipt 只存输入身份和接受位置，不复制事件正文。
// Terminal 为空表示尚未确认 Run 终态，不能据接受回执切换下一条后台命令。
type InputReceipt struct {
	Unsafe   bool   `json:"unsafe,omitempty"`
	InputID  string `json:"input_id"`
	RunID    string `json:"run_id"`
	EventID  string `json:"event_id,omitempty"`
	Sequence uint64 `json:"sequence,omitempty"`
	Steered  bool   `json:"steered,omitempty"`
	Terminal string `json:"terminal,omitempty"`
}

func (c *Client) SubmitInput(ctx context.Context, sessionID, inputID, text string) (InputReceipt, error) {
	in := struct {
		InputID string `json:"input_id"`
		Text    string `json:"text"`
	}{inputID, text}
	var out InputReceipt
	if err := c.post(ctx, sessionPath(sessionID)+"/inputs", in, &out); err != nil {
		return InputReceipt{}, err
	}
	if out.RunID == "" {
		return InputReceipt{}, errors.New("kernel: missing run_id")
	}
	out.InputID = inputID
	return out, nil
}

type inputCallEvidence struct {
	WritesWithoutResult      uint `json:"writes_without_result"`
	WritesWithError          uint `json:"writes_with_error"`
	ResumesWithPendingWrites uint `json:"resumes_with_pending_writes"`
}

type inputStatus struct {
	Found           *bool              `json:"found"`
	AsOfSeq         uint64             `json:"as_of_seq,string"`
	InputID         string             `json:"input_id"`
	RunID           string             `json:"run_id"`
	EventID         string             `json:"event_id"`
	AcceptedSeq     uint64             `json:"accepted_seq,string"`
	AcceptanceKind  string             `json:"acceptance_kind"`
	AnsweredCallID  string             `json:"answered_call_id"`
	RunStatus       string             `json:"run_status"`
	TakeoverCount   uint               `json:"takeover_count"`
	Calls           *inputCallEvidence `json:"calls"`
	SinceAcceptance *struct {
		AttemptsWithoutPriorSuspend uint               `json:"attempts_without_prior_suspend"`
		Calls                       *inputCallEvidence `json:"calls"`
	} `json:"since_acceptance"`
}

func (c *Client) inputStatus(ctx context.Context, sessionID, inputID string) (inputStatus, error) {
	query := url.Values{"input_id": {inputID}}
	var out inputStatus
	err := c.request(ctx, http.MethodGet, sessionPath(sessionID)+"/inputs?"+query.Encode(), nil, &out)
	return out, err
}

func (c *Client) FindInput(ctx context.Context, sessionID, inputID string) (InputReceipt, bool, error) {
	status, err := c.inputStatus(ctx, sessionID, inputID)
	if err != nil {
		var httpErr *HTTPError
		if errors.As(err, &httpErr) {
			// 旧路由只注册了 POST，Go mux 会返回 405。部署支持的最旧内核有查询路由后移除此判定。
			legacyRoute := httpErr.Code == "" && (httpErr.StatusCode == http.StatusNotFound || httpErr.allowsPost)
			// Session 日志只会增长。超预算后改走 Events，才能继续对账长 Session。
			if httpErr.Code == "scan_budget_exceeded" || legacyRoute {
				return c.findInputByEvents(ctx, sessionID, inputID)
			}
		}
		return InputReceipt{}, false, err
	}
	if status.Found == nil || status.AsOfSeq == 0 {
		return InputReceipt{}, false, errors.New("kernel: invalid input status")
	}
	if !*status.Found {
		return InputReceipt{}, false, nil
	}
	if status.InputID != inputID || status.RunID == "" || status.EventID == "" ||
		status.AcceptedSeq == 0 || status.AcceptedSeq > status.AsOfSeq || status.Calls == nil {
		return InputReceipt{}, false, errors.New("kernel: invalid input acceptance")
	}
	receipt := InputReceipt{InputID: inputID, RunID: status.RunID, EventID: status.EventID, Sequence: status.AcceptedSeq}
	switch status.RunStatus {
	case "queued", "running", "suspended":
	case "completed", "failed", "interrupted":
		receipt.Terminal = status.RunStatus
	default:
		return InputReceipt{}, false, errors.New("kernel: invalid input run status")
	}
	attempts, calls := status.TakeoverCount, status.Calls
	switch status.AcceptanceKind {
	case "requested":
	case "steered":
		if status.SinceAcceptance == nil || status.SinceAcceptance.Calls == nil {
			return InputReceipt{}, false, errors.New("kernel: missing evidence since input acceptance")
		}
		receipt.Steered = true
		attempts, calls = status.SinceAcceptance.AttemptsWithoutPriorSuspend, status.SinceAcceptance.Calls
	case "answered":
		if status.AnsweredCallID == "" {
			return InputReceipt{}, false, errors.New("kernel: missing answered call")
		}
		// 回答沿用提交回执的 Steered 语义。它推进的是既有 Run，不能据此确认后台命令可安全重做。
		receipt.Steered, receipt.Unsafe = true, true
	default:
		return InputReceipt{}, false, errors.New("kernel: invalid input acceptance kind")
	}
	receipt.Unsafe = receipt.Unsafe || attempts > 0 || calls.ResumesWithPendingWrites > 0 ||
		calls.WritesWithError > 0 || (receipt.Terminal != "" && calls.WritesWithoutResult > 0)
	return receipt, true, nil
}

type inputEvent struct {
	RunID   string `json:"run_id"`
	InputID string `json:"input_id"`
}

type controlEvent struct {
	RunID    string `json:"run_id"`
	Attempt  uint32 `json:"attempt"`
	CallID   string `json:"call_id"`
	ReadOnly bool   `json:"read_only"`
	IsError  bool   `json:"is_error"`
}

type event struct {
	Attempt     *controlEvent `json:"attempt_started"`
	Suspended   *inputEvent   `json:"run_suspended"`
	Call        *controlEvent `json:"tool_call_started"`
	Result      *controlEvent `json:"tool_result"`
	ID          string        `json:"id"`
	SessionID   string        `json:"session_id"`
	Seq         json.Number   `json:"seq"`
	Requested   *inputEvent   `json:"run_requested"`
	Steered     *inputEvent   `json:"steered"`
	Completed   *inputEvent   `json:"run_completed"`
	Failed      *inputEvent   `json:"run_failed"`
	Interrupted *inputEvent   `json:"run_interrupted"`
}

// findInputByEvents 从头读到空页；短页不是结束标记，最近 64 个输入也不是永久去重证据。
// 即使前页找到输入，后续页失败也不返回半份结果，避免漏掉终态或混淆删除竞态。
func (c *Client) findInputByEvents(ctx context.Context, sessionID, inputID string) (InputReceipt, bool, error) {
	var after uint64
	var receipt InputReceipt
	writes := map[string]bool{}
	suspended := false
	for {
		var page struct {
			Events *[]event `json:"events"`
		}
		query := url.Values{"after": {strconv.FormatUint(after, 10)}, "limit": {"32"}}
		// 单个工具结果可能超过普通 JSON 响应的上限；只解码控制字段，正文不复制进 receipt。
		if err := c.requestJSON(ctx, http.MethodGet, sessionPath(sessionID)+"/events?"+query.Encode(), nil, &page, 0); err != nil {
			return InputReceipt{}, false, err
		}
		if page.Events == nil {
			return InputReceipt{}, false, errors.New("kernel: missing events")
		}
		if len(*page.Events) == 0 {
			if receipt.Terminal != "" && len(writes) > 0 {
				receipt.Unsafe = true
			}
			return receipt, receipt.RunID != "", nil
		}
		for _, e := range *page.Events {
			seq, err := strconv.ParseUint(string(e.Seq), 10, 64)
			if err != nil || seq <= after || e.SessionID != sessionID {
				return InputReceipt{}, false, errors.New("kernel: invalid event cursor")
			}
			after = seq
			input := e.Requested
			if input == nil {
				input = e.Steered
			}
			if input != nil && input.InputID == inputID {
				if input.RunID == "" {
					return InputReceipt{}, false, errors.New("kernel: missing event run_id")
				}
				if receipt.RunID != "" && receipt.RunID != input.RunID {
					return InputReceipt{}, false, errors.New("kernel: conflicting input receipts")
				}
				receipt = InputReceipt{InputID: inputID, RunID: input.RunID, EventID: e.ID, Sequence: seq, Steered: e.Steered != nil}
			}
			if receipt.RunID == "" {
				continue
			}
			// 错误正文可能含内容，只读控制字段；接管与不明确的写结果保守隔离。
			if e.Suspended != nil && e.Suspended.RunID == receipt.RunID {
				suspended = true
			}
			if e.Attempt != nil && e.Attempt.RunID == receipt.RunID {
				// 正常移交、审批或配额恢复也增加 Attempt；只有未安全挂起的接管才隔离。
				if e.Attempt.Attempt > 1 && (!suspended || len(writes) > 0) {
					receipt.Unsafe = true
				}
				suspended = false
			}
			if e.Call != nil && e.Call.RunID == receipt.RunID && !e.Call.ReadOnly {
				writes[e.Call.CallID] = true
			}
			if e.Result != nil && e.Result.RunID == receipt.RunID && writes[e.Result.CallID] {
				if e.Result.IsError {
					receipt.Unsafe = true
				}
				delete(writes, e.Result.CallID)
			}
			if e.Completed != nil && e.Completed.RunID == receipt.RunID {
				receipt.Terminal = "completed"
			}
			if e.Failed != nil && e.Failed.RunID == receipt.RunID {
				receipt.Terminal = "failed"
			}
			if e.Interrupted != nil && e.Interrupted.RunID == receipt.RunID {
				receipt.Terminal = "interrupted"
			}
		}
	}
}
