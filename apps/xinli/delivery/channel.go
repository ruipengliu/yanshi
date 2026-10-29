package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"xinli/account"
	"xinli/kernel"
	"xinli/task"
)

type InboundKernel interface {
	Kernel
	SubmitInput(context.Context, string, string, string) (kernel.InputReceipt, error)
	FindInput(context.Context, string, string) (kernel.InputReceipt, bool, error)
}

type Inbound struct {
	Store    Store
	Accounts account.Store
	Kernel   InboundKernel
}

// Bind 由已鉴权的应用账号操作调用；供应商不能自行声明 EndUser 或更改绑定。
// P4 不挂供应商 webhook，真实供应商的签名与账号绑定流程属于外部接入阶段。
func (in *Inbound) Bind(ctx context.Context, owner task.Owner, channel, address string) error {
	if channel == "" || address == "" {
		return account.ErrInvalid
	}
	return in.Accounts.WithOwner(ctx, owner, func(ctx context.Context) error {
		return in.Store.UpdateDelivery(ctx, owner, func(state *State) error { state.Channels[channel] = address; return nil })
	})
}

// Message 仅接受普通文字；审批没有渠道执行路径，即使调用方提供有效 call_id 也不能批准。
func (in *Inbound) Message(ctx context.Context, owner task.Owner, channel, address, session, messageID, action, text string) (kernel.InputReceipt, error) {
	if action != "message" {
		return kernel.InputReceipt{}, ErrChannelApproval
	}
	if messageID == "" || text == "" || session == "" {
		return kernel.InputReceipt{}, account.ErrInvalid
	}
	var receipt kernel.InputReceipt
	err := in.Accounts.WithOwner(ctx, owner, func(ctx context.Context) error {
		state, err := in.Store.ReadDelivery(ctx, owner)
		if err != nil {
			return err
		}
		if state.Channels[channel] != address || address == "" {
			return account.ErrUnauthorized
		}
		view, err := in.Kernel.SessionView(ctx, owner.BusinessLine, session)
		if err != nil {
			return err
		}
		if view.ID != session || view.BusinessLine != owner.BusinessLine || view.EndUser != owner.EndUser || view.Agent.Name != "xinli-assistant" {
			return account.ErrUnauthorized
		}
		key := Identity(channel, address, messageID)
		inputID := "channel-" + key
		ctx = kernel.WithBusinessLine(ctx, owner.BusinessLine)
		if previous, exists := state.Inbound[key]; exists {
			if previous != session {
				return account.ErrInvalid
			}
			var found bool
			receipt, found, err = in.Kernel.FindInput(ctx, session, inputID)
			if err != nil {
				return err
			}
			if !found {
				return ErrUnknown
			}
			return nil
		}
		// 先保存消息 ID；超时之后只对账，不把内核最近 64 条输入去重当永久幂等。
		if err = in.Store.UpdateDelivery(ctx, owner, func(state *State) error { state.Inbound[key] = session; return nil }); err != nil {
			return err
		}
		receipt, err = in.Kernel.SubmitInput(ctx, session, inputID, text)
		if err != nil {
			return errors.Join(ErrUnknown, err)
		}
		return nil
	})
	return receipt, err
}

// 回复文字只在发送时临时读取，不存进任务；推理过程和工具参数永不转发。
func (w *Worker) channelText(ctx context.Context, owner task.Owner, job Job) (string, error) {
	if job.Item.Kind != "run_completed" {
		return ChannelText(job.Item.Kind), nil
	}
	var after uint64
	text := ""
	for {
		page, err := w.Kernel.Events(ctx, owner.BusinessLine, job.Item.SessionID, after, 100)
		if err != nil {
			return "", err
		}
		for _, raw := range page.Events {
			var event struct {
				Seq     json.Number `json:"seq"`
				Message *struct {
					RunID   string            `json:"run_id"`
					Calls   []json.RawMessage `json:"tool_calls"`
					Content []struct {
						Text *struct {
							Text string `json:"text"`
						} `json:"text"`
					} `json:"content"`
				} `json:"assistant_message"`
			}
			if err := json.Unmarshal(raw, &event); err != nil {
				return "", err
			}
			seq, err := strconv.ParseUint(string(event.Seq), 10, 64)
			if err != nil {
				return "", err
			}
			if seq > job.Item.Seq {
				break
			}
			m := event.Message
			if m != nil && m.RunID == job.Item.RunID && len(m.Calls) == 0 {
				parts := []string{}
				for _, block := range m.Content {
					if block.Text != nil {
						parts = append(parts, block.Text.Text)
					}
				}
				text = strings.Join(parts, "\n")
			}
		}
		if len(page.Events) == 0 || page.Last >= job.Item.Seq {
			if text == "" {
				return ChannelText(job.Item.Kind), nil
			}
			return text + "\n\n卡片与完整内容请打开 App 查看。", nil
		}
		if page.Last <= after {
			return "", errors.New("delivery: invalid events progress")
		}
		after = page.Last
	}
}
