// Package delivery 保存应用收件箱与通知任务；正文始终从当前可访问的数据源读取。
package delivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"time"

	"xinli/kernel"
	"xinli/task"
)

var ErrNotFound = errors.New("delivery: not found")

// Item 只保存定位正文所需的 ID；At 是应用首次处理接收记录的时间，不冒充事件发生时间。
type Item struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	Seq       uint64    `json:"seq"`
	Kind      string    `json:"kind"`
	RunID     string    `json:"run_id,omitempty"`
	CallID    string    `json:"call_id,omitempty"`
	At        time.Time `json:"at"`
	Read      bool      `json:"read"`
}

type Job struct {
	ID           string
	Item         Item
	CommitmentID string
	Revision     int
	DueDate      string
	State        string
	RetryAt      time.Time
	Attempts     int
}

// State 是一个 owner 的应用短事务；发送在事务外，账号 gate 串行化发送者。
type State struct {
	Items    map[string]Item
	Jobs     map[string]Job
	Inbound  map[string]string
	Channels map[string]string
}

func (s *State) Init() {
	if s.Items == nil {
		s.Items = map[string]Item{}
	}
	if s.Jobs == nil {
		s.Jobs = map[string]Job{}
	}
	if s.Inbound == nil {
		s.Inbound = map[string]string{}
	}
	if s.Channels == nil {
		s.Channels = map[string]string{}
	}
}

func Clone(s State) State {
	data, _ := json.Marshal(s)
	var out State
	_ = json.Unmarshal(data, &out)
	out.Init()
	return out
}

func Identity(parts ...string) string {
	data, _ := json.Marshal(parts)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s *State) Receive(e kernel.Envelope, now time.Time) {
	s.Init()
	if e.Agent != "xinli-assistant" {
		return
	}
	if e.Kind != "run_completed" && e.Kind != "question" && e.Kind != "approval_requested" {
		return
	}
	id := Identity(e.BusinessLine, e.SessionID, strconv.FormatUint(e.Seq, 10))
	if _, ok := s.Items[id]; ok {
		return
	}
	item := Item{ID: id, SessionID: e.SessionID, Seq: e.Seq, Kind: e.Kind, RunID: e.RunID, CallID: e.CallID, At: now}
	s.Items[id] = item
	s.Jobs[id] = Job{ID: id, Item: item, State: "pending", RetryAt: now}
}

func (s *State) Forget(session string) {
	for id, item := range s.Items {
		if item.SessionID == session {
			delete(s.Items, id)
		}
	}
	// 删除相关 ID，而非保留含 Session 的取消任务；Gone 屏障由接收表保存。
	for id, job := range s.Jobs {
		if job.Item.SessionID == session {
			delete(s.Jobs, id)
		}
	}
	for id, target := range s.Inbound {
		if target == session {
			delete(s.Inbound, id)
		}
	}
}

func (s *State) List() []Item {
	items := []Item{}
	for _, item := range s.Items {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items
}

type Store interface {
	// Pending 与 Process 分开，接收后崩溃仍从未处理行继续；Process 必须幂等且和建任务原子。
	Pending(context.Context, string, int) ([]kernel.Envelope, error)
	Process(context.Context, kernel.Envelope, time.Time) error
	ReadDelivery(context.Context, task.Owner) (State, error)
	UpdateDelivery(context.Context, task.Owner, func(*State) error) error
}
