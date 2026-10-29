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

type inputEvent struct {
	RunID   string `json:"run_id"`
	InputID string `json:"input_id"`
}

type event struct {
	ID          string      `json:"id"`
	SessionID   string      `json:"session_id"`
	Seq         json.Number `json:"seq"`
	Requested   *inputEvent `json:"run_requested"`
	Steered     *inputEvent `json:"steered"`
	Completed   *inputEvent `json:"run_completed"`
	Failed      *inputEvent `json:"run_failed"`
	Interrupted *inputEvent `json:"run_interrupted"`
}

// FindInput 从头读到空页；短页不是结束标记，最近 64 个输入也不是永久去重证据。
// 即使前页找到输入，后续页失败也不返回半份结果，避免漏掉终态或混淆删除竞态。
func (c *Client) FindInput(ctx context.Context, sessionID, inputID string) (InputReceipt, bool, error) {
	var after uint64
	var receipt InputReceipt
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
