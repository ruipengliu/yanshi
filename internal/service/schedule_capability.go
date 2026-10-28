package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/capability"
	"yanshi/internal/model"
	"yanshi/internal/schedule"
	"yanshi/internal/session"
)

// ScheduleCapability 经与 HTTP 相同的入口写入，能力白名单仍由 Catalog 检查。
func (s *Service) ScheduleCapability() capability.Capability { return scheduleCapability{s} }

type scheduleCapability struct{ service *Service }

type scheduleArguments struct {
	Action     string `json:"action"`
	Input      string `json:"input"`
	At         string `json:"at"`
	RRule      string `json:"rrule"`
	TimeZone   string `json:"time_zone"`
	Until      string `json:"until"`
	ScheduleID string `json:"schedule_id"`
}

func (c scheduleCapability) Spec() capability.Spec {
	return capability.Spec{
		Name: "schedule",
		Description: "用户要求按时做某事或提醒时使用。create 创建定时，list 列出有效定时，cancel 取消定时；取消前先 list 确认 ID。" +
			"input 写到点时要做的事。一次性用 at，周期用 rrule，二者只填一个；时间均为当地时间。time_zone 用 IANA 时区，省略时取当前会话的时区，当前会话没有时区时必须填写。" +
			"RRULE 支持 FREQ=DAILY|WEEKLY|MONTHLY、INTERVAL、BYDAY、BYMONTHDAY、BYHOUR、BYMINUTE、COUNT、UNTIL，必须带 BYHOUR 与 BYMINUTE，最小间隔一小时。" +
			"例如每周五 17:00：FREQ=WEEKLY;BYDAY=FR;BYHOUR=17;BYMINUTE=0；每天 08:30：FREQ=DAILY;BYHOUR=8;BYMINUTE=30。" +
			"until 可限制最后执行时刻，用带时区偏移的时间戳。子任务中不能创建定时。",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["create","list","cancel"],"description":"创建、列出或取消定时"},"input":{"type":"string","description":"create 必填：到点时要做的事，只写文字"},"at":{"type":"string","description":"一次性的当地时间，例如 2026-10-30T17:00，不带时区偏移；与 rrule 二选一"},"rrule":{"type":"string","description":"周期 RRULE 子集，必须带 BYHOUR 与 BYMINUTE；与 at 二选一"},"time_zone":{"type":"string","description":"IANA 时区，例如 Asia/Shanghai；省略时取当前会话的时区"},"until":{"type":"string","description":"最后执行时刻，例如 2026-12-31T23:59:00+08:00（可选）"},"schedule_id":{"type":"string","description":"cancel 必填：先 list 得到的定时 ID"}},"required":["action"],"additionalProperties":false}`),
		Idempotent:  true,
	}
}

// scheduleCallID 由 call_id 派生，重试同一调用得到同一 ID。取哈希的前 8 字节：同一 Session 内足够唯一，
// 又短到模型能照抄（模型要在 cancel 中原样写回它）。
func scheduleCallID(callID string) string {
	sum := sha256.Sum256([]byte(callID))
	return fmt.Sprintf("sch_%x", sum[:8])
}

func (c scheduleCapability) parse(inv capability.Invocation) (scheduleArguments, CreateScheduleRequest, error) {
	var in scheduleArguments
	invalid := func(reason string) (scheduleArguments, CreateScheduleRequest, error) {
		return in, CreateScheduleRequest{}, fmt.Errorf("%w: %s", schedule.ErrInvalid, reason)
	}
	if json.Unmarshal([]byte(inv.Arguments), &in) != nil {
		return invalid("参数须为 JSON 对象，请填写 action: create、list 或 cancel")
	}
	req := CreateScheduleRequest{ScheduleID: scheduleCallID(inv.CallID), Input: model.TextBlocks(in.Input), At: in.At, RRule: in.RRule, TimeZone: in.TimeZone, By: "agent"}
	switch in.Action {
	case "list":
		return in, req, nil
	case "cancel":
		if strings.TrimSpace(in.ScheduleID) == "" || len(in.ScheduleID) > 128 || strings.ContainsAny(in.ScheduleID, "\r\n\x00") {
			return invalid("取消前请先 list，再填写 schedule_id")
		}
		return in, req, nil
	case "create":
		if inv.State != nil {
			if inv.State.Created.GetParent() != nil {
				return invalid("子任务中不能创建定时")
			}
			if req.TimeZone == "" {
				req.TimeZone = inv.State.Created.GetTimeZone()
			}
		}
		if strings.TrimSpace(in.Input) == "" {
			return invalid("create 须填写 input，说明到点时要做的事")
		}
		if req.TimeZone == "" {
			return invalid("定时与会话均未设置 time_zone，请填写 IANA 时区，例如 Asia/Shanghai")
		}
		if in.Until != "" {
			var err error
			req.Until, err = time.Parse(time.RFC3339, in.Until)
			if err != nil {
				return invalid("until 须为带时区偏移的时间戳，例如 2026-12-31T23:59:00+08:00")
			}
		}
		return in, req, nil
	default:
		return invalid("action 须为 create、list 或 cancel")
	}
}

func (c scheduleCapability) Check(inv capability.Invocation) error {
	in, req, err := c.parse(inv)
	if err != nil || in.Action != "create" {
		return err
	}
	_, err = schedule.Compile(schedule.Spec{At: req.At, RRule: req.RRule, TimeZone: req.TimeZone, Until: req.Until}, c.service.Store.Clock.Now())
	if err == nil && inv.State != nil && len(inv.State.OpenSchedules()) >= session.MaxSchedules {
		return fmt.Errorf("%w: 每个会话至多有 %d 个有效定时，请先 list 并取消不再需要的定时", schedule.ErrInvalid, session.MaxSchedules)
	}
	return err
}

// ApprovalSummary 只描述动作；是否受外部内容影响由 Worker 的 tainted 统一判断。
func (c scheduleCapability) ApprovalSummary(inv capability.Invocation) string {
	in, req, err := c.parse(inv)
	if err != nil {
		return ""
	}
	const suffix = "（这次对话包含不是你直接说的内容，需要你确认）"
	switch in.Action {
	case "create":
		p, err := schedule.Compile(schedule.Spec{At: req.At, RRule: req.RRule, TimeZone: req.TimeZone, Until: req.Until}, c.service.Store.Clock.Now())
		if err != nil {
			return ""
		}
		next, _ := p.Next(p.Start())
		text, _ := model.Truncate(in.Input, 200)
		rule := ""
		if req.RRule != "" {
			rule = p.Spec().RRule + "，下次 "
		}
		return fmt.Sprintf("助手想创建定时：%s%s（%s）「%s」%s", rule, schedule.FormatLocal(next, p.Loc(), p.Start()), req.TimeZone, text, suffix)
	case "cancel":
		text := ""
		if inv.State != nil {
			if item := inv.State.Schedules[in.ScheduleID]; item != nil {
				text, _ = model.Truncate(model.Text(item.Created.GetInput()), 200)
			}
		}
		return fmt.Sprintf("助手想取消定时 %s「%s」%s", in.ScheduleID, text, suffix)
	}
	return ""
}

// scheduleLocal 给模型看的下次到期总带年份：模型对"今年是哪一年"的印象不可靠，省略年份时它会自己补错（评测发现）。
func scheduleLocal(item *session.Schedule) string {
	loc, err := time.LoadLocation(item.Created.GetTimeZone())
	if err != nil {
		// 历史时区可能退出目录，仍可用已保存的 UTC 告诉用户何时到期。
		return item.NextDue.UTC().Format(time.RFC3339)
	}
	return schedule.FormatLocal(item.NextDue, loc, time.Time{}) + "（" + item.Created.GetTimeZone() + "）"
}

func (c scheduleCapability) Invoke(ctx context.Context, inv capability.Invocation) ([]*v1.ContentBlock, error) {
	in, req, err := c.parse(inv)
	if err != nil {
		return nil, err
	}
	var result string
	switch in.Action {
	case "create":
		var item *session.Schedule
		item, err = c.service.CreateSchedule(ctx, inv.SessionID, req)
		if err == nil {
			if item.NextDue.IsZero() {
				result = "定时 " + item.Created.GetScheduleId() + " 已创建过，现已失效，没有重复创建"
			} else {
				result = "已创建定时 " + item.Created.GetScheduleId() + "：下次在 " + scheduleLocal(item)
			}
		}
	case "list":
		var items []*session.Schedule
		items, err = c.service.ListSchedules(ctx, inv.SessionID)
		result = "没有定时"
		if err == nil && len(items) > 0 {
			var b strings.Builder
			for _, item := range items {
				rule := "一次性"
				if item.Created.GetRrule() != "" {
					rule = item.Created.GetRrule()
				}
				fmt.Fprintf(&b, "- %s「%s」：%s，下次在 %s\n", item.Created.GetScheduleId(), model.Text(item.Created.GetInput()), rule, scheduleLocal(item))
			}
			result = strings.TrimSuffix(b.String(), "\n")
		}
	case "cancel":
		err = c.service.CancelSchedule(ctx, inv.SessionID, in.ScheduleID, "agent")
		if errors.Is(err, ErrNotFound) {
			// 取消本来就无效的定时也是目标状态，重试不会产生第二条事件。
			err = nil
			result = "定时 " + in.ScheduleID + " 已失效或不存在"
		} else {
			result = "已取消定时 " + in.ScheduleID
		}
	}
	if err != nil {
		if errors.Is(err, schedule.ErrInvalid) {
			return nil, err
		}
		if errors.Is(err, ErrInvalid) {
			return nil, fmt.Errorf("%w: %s", schedule.ErrInvalid, strings.TrimPrefix(err.Error(), ErrInvalid.Error()+": "))
		}
		return nil, errors.New("定时操作未完成，请稍后 list 核对后重试")
	}
	return model.TextBlocks(result), nil
}
