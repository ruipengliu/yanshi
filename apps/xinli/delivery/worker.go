package delivery

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"time"

	"persona/domain"
	"persona/paging"
	"persona/query"
	"persona/store"
	"xinli/account"
	"xinli/kernel"
	"xinli/task"
)

type Kernel interface {
	SessionView(context.Context, string, string) (kernel.SessionView, error)
	Events(context.Context, string, string, uint64, int) (kernel.EventPage, error)
}

type Preferences struct {
	Channel    string `json:"channel"`
	QuietStart string `json:"quiet_start"`
	QuietEnd   string `json:"quiet_end"`
}

// Preferences 位于账号 preferences.notifications；默认只走应用推送，off 明确关闭。
func ReadPreferences(raw json.RawMessage) (Preferences, error) {
	out := Preferences{Channel: "push"}
	var root struct {
		Notifications json.RawMessage `json:"notifications"`
	}
	if len(raw) > 0 && json.Unmarshal(raw, &root) != nil {
		return out, account.ErrInvalid
	}
	if len(root.Notifications) > 0 && json.Unmarshal(root.Notifications, &out) != nil {
		return out, account.ErrInvalid
	}
	if out.Channel != "push" && out.Channel != "channel" && out.Channel != "off" {
		return out, account.ErrInvalid
	}
	if (out.QuietStart == "") != (out.QuietEnd == "") {
		return out, account.ErrInvalid
	}
	if out.QuietStart != "" {
		if _, err := time.Parse("15:04", out.QuietStart); err != nil {
			return out, account.ErrInvalid
		}
		if _, err := time.Parse("15:04", out.QuietEnd); err != nil {
			return out, account.ErrInvalid
		}
	}
	return out, nil
}

func (p Preferences) Quiet(now time.Time, zone string) (bool, error) {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return false, err
	}
	if p.QuietStart == "" || p.QuietStart == p.QuietEnd {
		return false, nil
	}
	clock := now.In(loc).Format("15:04")
	if p.QuietStart < p.QuietEnd {
		return clock >= p.QuietStart && clock < p.QuietEnd, nil
	}
	return clock >= p.QuietStart || clock < p.QuietEnd, nil
}

type Worker struct {
	Store    Store
	Accounts account.Store
	Persona  query.Reader
	Kernel   Kernel
	Push     Push
	Channel  Channel
	Now      func() time.Time
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// Run 在账号 gate 内做复核与发送，SQL 事务只用于开始/完成两个短提交。
// 发送结果未知或成功后回写失败都可能重复触达，不宣称供应商恰好一次。
func (w *Worker) Run(ctx context.Context, owner task.Owner, id string) error {
	return w.Accounts.WithOwner(ctx, owner, func(ctx context.Context) error {
		record, err := w.Accounts.ReadAccount(ctx, owner)
		if err != nil {
			return err
		}
		if record.Account.Status != account.Active {
			return nil
		}
		state, err := w.Store.ReadDelivery(ctx, owner)
		if err != nil {
			return err
		}
		job, ok := state.Jobs[id]
		if !ok || job.State == "sent" || job.State == "cancelled" || job.RetryAt.After(w.now()) {
			return nil
		}
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		prefs, err := ReadPreferences(record.Account.Preferences)
		if err != nil {
			return w.retry(ctx, owner, job, err)
		}
		relevant, err := w.relevant(ctx, owner, job)
		if err != nil {
			return w.retry(ctx, owner, job, err)
		}
		if !relevant || prefs.Channel == "off" {
			job.State = "cancelled"
			return w.save(ctx, owner, job)
		}
		quiet, err := prefs.Quiet(w.now(), record.Account.TimeZone)
		if err != nil {
			return w.retry(ctx, owner, job, err)
		}
		if quiet {
			job.RetryAt = w.now().Add(time.Minute)
			return w.save(ctx, owner, job)
		}
		job.Attempts++
		job.State, job.RetryAt = "sending", w.now().Add(task.Backoff(job.ID, job.Attempts, time.Second, 10*time.Minute))
		if err := w.save(ctx, owner, job); err != nil {
			return err
		}
		// 再查应用删除屏障；后台删除可以在网络复核期间提交。
		latest, err := w.Store.ReadDelivery(ctx, owner)
		if err != nil {
			return err
		}
		if _, exists := latest.Jobs[id]; !exists {
			return nil
		}
		message := PushMessage{Kind: job.Item.Kind, ID: job.ID, SessionID: job.Item.SessionID, RunID: job.Item.RunID, CallID: job.Item.CallID}
		message.DeepLink = "xinli://threads/" + url.PathEscape(job.Item.SessionID)
		if job.CommitmentID != "" {
			message.Kind = "commitment_due"
			message.DeepLink = "xinli://commitments/" + url.PathEscape(job.CommitmentID)
		}
		if prefs.Channel == "channel" {
			keys := make([]string, 0, len(latest.Channels))
			for key := range latest.Channels {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			if w.Channel == nil || len(keys) == 0 {
				return w.retry(ctx, owner, job, errors.New("delivery: channel unavailable"))
			}
			text := ChannelText(message.Kind)
			if job.CommitmentID == "" {
				text, err = w.channelText(ctx, owner, job)
				if err != nil {
					return w.retry(ctx, owner, job, err)
				}
				// 读取正文可能分页，最后再复核，避免在读页期间已重答或处理的事项被发送。
				relevant, err = w.relevant(ctx, owner, job)
				if err != nil {
					return w.retry(ctx, owner, job, err)
				}
				if !relevant {
					job.State = "cancelled"
					return w.save(ctx, owner, job)
				}
			}
			err = w.Channel.Send(ctx, owner, latest.Channels[keys[0]], ChannelMessage{ID: job.ID, Text: text, DeepLink: message.DeepLink})
		} else {
			if w.Push == nil {
				return w.retry(ctx, owner, job, errors.New("delivery: push unavailable"))
			}
			err = w.Push.Push(ctx, owner, message)
		}
		if err != nil {
			return w.retry(ctx, owner, job, err)
		}
		job.State = "sent"
		return w.save(ctx, owner, job)
	})
}

func ChannelText(kind string) string {
	switch kind {
	case "approval_requested":
		return "有待审批事项，请回到 App 查看并决定。"
	case "question":
		return "助手有问题需要你回答，请打开 App。"
	case "commitment_due":
		return "有承诺到期，请打开 App 查看当前安排。"
	default:
		return "助手有新回复，文字与卡片请打开 App 查看。"
	}
}

func (w *Worker) save(ctx context.Context, owner task.Owner, job Job) error {
	return w.Store.UpdateDelivery(ctx, owner, func(state *State) error {
		if _, exists := state.Jobs[job.ID]; exists {
			state.Jobs[job.ID] = job
		}
		return nil
	})
}

func (w *Worker) retry(ctx context.Context, owner task.Owner, job Job, cause error) error {
	if job.State != "sending" {
		job.Attempts++
	}
	job.State = "pending"
	job.RetryAt = w.now().Add(task.Backoff(job.ID, job.Attempts, time.Second, 10*time.Minute))
	// 请求预算耗尽也要保存退避；账号 gate 的连接仍在回调内使用。
	persist, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return errors.Join(cause, w.save(persist, owner, job))
}

func (w *Worker) relevant(ctx context.Context, owner task.Owner, job Job) (bool, error) {
	if job.CommitmentID != "" {
		return w.commitmentCurrent(ctx, owner, job)
	}
	if w.Kernel == nil {
		return false, errors.New("delivery: kernel unavailable")
	}
	view, err := w.Kernel.SessionView(ctx, owner.BusinessLine, job.Item.SessionID)
	if errors.Is(err, kernel.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if view.ID != job.Item.SessionID || view.BusinessLine != owner.BusinessLine || view.EndUser != owner.EndUser || view.Agent.Name != "xinli-assistant" {
		return false, errors.New("delivery: owner mismatch")
	}
	if job.Item.Kind == "question" || job.Item.Kind == "approval_requested" {
		kind := "question"
		if job.Item.Kind == "approval_requested" {
			kind = "approval"
		}
		for _, run := range view.Runs {
			if run.ID == job.Item.RunID && run.Waiting != nil && run.Waiting.Kind == kind && run.Waiting.CallID == job.Item.CallID {
				return run.Waiting.Deadline.IsZero() || w.now().Before(run.Waiting.Deadline), nil
			}
		}
		return false, nil
	}
	// 重答接受就替代；不能以新 Run 是否成功或是否仍在有限的 runs 投影内判断。
	var after uint64
	for {
		page, err := w.Kernel.Events(ctx, owner.BusinessLine, job.Item.SessionID, after, 100)
		if errors.Is(err, kernel.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		for _, raw := range page.Events {
			var event struct {
				Requested *struct {
					RedoOf string `json:"redo_of"`
				} `json:"run_requested"`
			}
			if err := json.Unmarshal(raw, &event); err != nil {
				return false, err
			}
			if event.Requested != nil && event.Requested.RedoOf == job.Item.RunID {
				return false, nil
			}
		}
		if len(page.Events) == 0 {
			return true, nil
		}
		if page.Last <= after {
			return false, errors.New("delivery: invalid events progress")
		}
		after = page.Last
	}
}

func (w *Worker) commitments(ctx context.Context, owner task.Owner, fn func(query.CommitmentView)) error {
	if w.Persona == nil {
		return errors.New("delivery: persona unavailable")
	}
	cursor := ""
	var state domain.State
	for {
		raw, err := w.Persona.Read(ctx, store.Owner{BusinessLine: owner.BusinessLine, EndUser: owner.EndUser}, query.Request{Resource: "commitments", Limit: 100, Cursor: cursor})
		if err != nil {
			return err
		}
		var page paging.Page[query.CommitmentView]
		if err := json.Unmarshal(raw, &page); err != nil {
			return err
		}
		for _, item := range page.Items {
			state.Commitments = append(state.Commitments, item.Commitment)
		}
		if page.NextCursor == "" {
			// 提醒集合由 Persona 领域规则选择，不能提前按日期截断可能重复的承诺。
			for _, item := range state.Reminders() {
				fn(query.CommitmentView{Commitment: item, Rev: item.Rev()})
			}
			return nil
		}
		if page.NextCursor == cursor {
			return errors.New("delivery: invalid commitment cursor")
		}
		cursor = page.NextCursor
	}
}

func (w *Worker) commitmentCurrent(ctx context.Context, owner task.Owner, job Job) (bool, error) {
	valid := false
	err := w.commitments(ctx, owner, func(item query.CommitmentView) {
		if item.ID == job.CommitmentID && item.Rev == job.Revision && item.DueDate == job.DueDate && item.Status == "open" && item.MergedInto == "" {
			valid = true
		}
	})
	return valid, err
}

// ScanOwner 从当前事实产生到期任务，旧版本保留为取消状态，重复扫描不重发。
func (w *Worker) ScanOwner(ctx context.Context, owner task.Owner) error {
	return w.Accounts.WithOwner(ctx, owner, func(ctx context.Context) error {
		record, err := w.Accounts.ReadAccount(ctx, owner)
		if err != nil {
			return err
		}
		if record.Account.Status != account.Active {
			return nil
		}
		loc, err := time.LoadLocation(record.Account.TimeZone)
		if err != nil {
			return err
		}
		today := w.now().In(loc).Format(time.DateOnly)
		jobs := []Job{}
		err = w.commitments(ctx, owner, func(item query.CommitmentView) {
			if item.Status != "open" || item.MergedInto != "" || item.DueDate == "" || item.DueDate > today {
				return
			}
			id := Identity("commitment", item.ID, strconv.Itoa(item.Rev))
			jobs = append(jobs, Job{ID: id, CommitmentID: item.ID, Revision: item.Rev, DueDate: item.DueDate, State: "pending", RetryAt: w.now()})
		})
		if err != nil {
			return err
		}
		if err = w.Store.UpdateDelivery(ctx, owner, func(state *State) error {
			for _, job := range jobs {
				if _, exists := state.Jobs[job.ID]; !exists {
					state.Jobs[job.ID] = job
				}
			}
			return nil
		}); err != nil {
			return err
		}
		state, err := w.Store.ReadDelivery(ctx, owner)
		if err != nil {
			return err
		}
		ids := []string{}
		for id, job := range state.Jobs {
			if job.State != "sent" && job.State != "cancelled" && !job.RetryAt.After(w.now()) {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		var result error
		for _, id := range ids[:min(len(ids), 100)] {
			result = errors.Join(result, w.Run(ctx, owner, id))
		}
		return result
	})
}
