package outbox

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"yanshi/internal/clock"
)

const Retention = 72 * time.Hour

var (
	ErrInvalid = errors.New("outbox_invalid_request")
	ErrCursor  = errors.New("outbox_invalid_cursor")
	ErrScope   = errors.New("outbox_cursor_scope_mismatch")
	ErrFilter  = errors.New("outbox_filter_changed")
	ErrEpoch   = errors.New("outbox_epoch_changed")
	ErrExpired = errors.New("outbox_cursor_expired")
	ErrShard   = errors.New("outbox_unknown_shard")
)

type Request struct {
	BusinessLine string
	Shard        int
	After        string
	Kinds        []Kind
	Limit        int
}

type Page struct {
	Events []Envelope `json:"events"`
	Next   string     `json:"next"`
}

// Position 的 ID=0 表示已扫描到 XID 的开头；真实行号从 1 起，不能跳过该事务。
type Position struct {
	XID uint64 `json:"xid8"`
	ID  uint64 `json:"id"`
}

func (p Position) Before(q Position) bool {
	return p.XID < q.XID || p.XID == q.XID && p.ID < q.ID
}

// StreamState 与信封在同一读取快照中取得。Target 在分批清理期间阻止读到残缺历史。
type StreamState struct {
	Epoch  string
	Key    []byte
	Floor  Position
	Target Position
}

type cursor struct {
	Version      int    `json:"v"`
	BusinessLine string `json:"business_line"`
	Shard        int    `json:"shard"`
	Epoch        string `json:"stream_epoch"`
	Filter       string `json:"filter_version"`
	Position
}

func NewStreamState() StreamState {
	var b [48]byte
	// 仅在存储创建或显式重建时生成，不消耗 Worker 的确定性 ID 序列。
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return StreamState{Epoch: hex.EncodeToString(b[:16]), Key: slices.Clone(b[16:])}
}

func (r Request) Normalize() (Request, error) {
	if r.BusinessLine == "" || r.Limit < 0 || r.Limit > 1000 {
		return r, ErrInvalid
	}
	if r.Shard < 0 {
		return r, ErrShard
	}
	if r.Limit == 0 {
		r.Limit = 100
	}
	r.Kinds = slices.Clone(r.Kinds)
	if len(r.Kinds) == 0 {
		r.Kinds = []Kind{RunCompleted, RunFailed, Question, ApprovalRequested, BackgroundSettled, SessionClosed, SessionDeleted}
	}
	for _, k := range r.Kinds {
		switch k {
		case RunCompleted, RunFailed, Question, ApprovalRequested, BackgroundSettled, SessionClosed, SessionDeleted:
		default:
			return r, ErrInvalid
		}
	}
	slices.Sort(r.Kinds)
	r.Kinds = slices.Compact(r.Kinds)
	return r, nil
}

func (r Request) filter() string {
	kinds := make([]string, len(r.Kinds))
	for i, k := range r.Kinds {
		kinds[i] = string(k)
	}
	h := sha256.Sum256([]byte("1:" + strings.Join(kinds, ",")))
	return hex.EncodeToString(h[:])
}

func (s StreamState) Start(r Request) (Position, error) {
	floor := s.Floor
	if floor.Before(s.Target) {
		floor = s.Target
	}
	if r.After == "" {
		return floor, nil
	}
	if len(r.After) > 4096 {
		return Position{}, ErrCursor
	}
	body, signature, ok := strings.Cut(r.After, ".")
	b, err := base64.RawURLEncoding.DecodeString(body)
	if !ok || err != nil {
		return Position{}, ErrCursor
	}
	var c cursor
	if json.Unmarshal(b, &c) != nil || c.Version != 1 {
		return Position{}, ErrCursor
	}
	if c.Shard != r.Shard {
		return Position{}, ErrScope
	}
	if c.Epoch != s.Epoch {
		return Position{}, ErrEpoch
	}
	mac := hmac.New(sha256.New, s.Key)
	mac.Write(b)
	sig, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || !hmac.Equal(sig, mac.Sum(nil)) {
		return Position{}, ErrCursor
	}
	if c.BusinessLine != r.BusinessLine || c.Shard != r.Shard {
		return Position{}, ErrScope
	}
	if c.Filter != r.filter() {
		return Position{}, ErrFilter
	}
	if c.Position.Before(floor) {
		return Position{}, ErrExpired
	}
	return c.Position, nil
}

func (s StreamState) Encode(r Request, p Position) string {
	b, _ := json.Marshal(cursor{Version: 1, BusinessLine: r.BusinessLine, Shard: r.Shard,
		Epoch: s.Epoch, Filter: r.filter(), Position: p})
	mac := hmac.New(sha256.New, s.Key)
	mac.Write(b)
	return base64.RawURLEncoding.EncodeToString(b) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Reader 返回安全水位以下的匹配行；Head 是恢复扫描前保存的安全起点 H。
type Reader interface {
	Read(context.Context, Request) (Page, error)
	Head(context.Context, Request) (string, error)
	Subscribe(businessLine string) (<-chan struct{}, func())
}

// Maintainer 每轮记录一次水位并最多清理 batch 行。停机或失败只延长保留，不提前过期。
type Maintainer interface {
	Maintain(ctx context.Context, clk clock.Clock, batch int) error
	Stats(context.Context) (Stats, error)
}

type Stats struct {
	WatermarkLag      uint64
	OldestTransaction time.Duration
	Rows              int64
	Bytes             int64
}

// Shards 是配置枚举，即使某片不可用也不从列表移除；Head 的失败只改变该片状态。
type Shards interface {
	ShardIDs() []int
	Slots(int) []int
	Identity() (string, string)
	SubscribeShard(int, string) (<-chan struct{}, func())
}
