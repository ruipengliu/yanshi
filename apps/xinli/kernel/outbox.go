package kernel

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Envelope 是公开出站信封的无正文元数据；恢复等价物没有 EnvelopeID，以 SessionID/Seq 去重。
type Envelope struct {
	EnvelopeID        string `json:"envelope_id,omitempty"`
	BusinessLine      string `json:"business_line"`
	EndUser           string `json:"end_user,omitempty"`
	SessionID         string `json:"session_id"`
	Seq               uint64 `json:"seq,omitempty"`
	Kind              string `json:"kind"`
	RunID             string `json:"run_id,omitempty"`
	CallID            string `json:"call_id,omitempty"`
	Agent             string `json:"agent,omitempty"`
	AgentVersion      string `json:"agent_version,omitempty"`
	Reason            string `json:"reason,omitempty"`
	DeletionRequestID string `json:"deletion_request_id,omitempty"`
}

type OutboxShard struct {
	Shard  int    `json:"shard"`
	Status string `json:"status"`
	Next   string `json:"next"`
}

type OutboxPage struct {
	Events []Envelope `json:"events"`
	Next   string     `json:"next"`
}

func OutboxResetRequired(err error) bool {
	var e *HTTPError
	return errors.As(err, &e) && e.StatusCode == http.StatusGone &&
		(e.Reason == "outbox_cursor_expired" || e.Reason == "outbox_epoch_changed")
}

func (c *Client) OutboxHeads(ctx context.Context, line string) ([]OutboxShard, error) {
	return c.OutboxHeadsKinds(ctx, line, nil)
}

func (c *Client) OutboxHeadsKinds(ctx context.Context, line string, kinds []string) ([]OutboxShard, error) {
	var out struct {
		Shards []OutboxShard `json:"shards"`
	}
	q := url.Values{"business_line": {line}}
	if len(kinds) > 0 {
		q.Set("kinds", strings.Join(kinds, ","))
	}
	if err := c.request(WithBusinessLine(ctx, line), http.MethodGet, "/v1/outbox/shards?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	if len(out.Shards) == 0 {
		return nil, errors.New("kernel: missing outbox shards")
	}
	for _, s := range out.Shards {
		if s.Shard < 0 || seen[s.Shard] || s.Status != "ready" || s.Next == "" {
			return nil, errors.New("kernel: unavailable outbox shard")
		}
		seen[s.Shard] = true
	}
	return out.Shards, nil
}

func (c *Client) Outbox(ctx context.Context, line string, shard int, after string, limit int) (OutboxPage, error) {
	return c.OutboxKinds(ctx, line, shard, after, limit, nil)
}

func (c *Client) OutboxKinds(ctx context.Context, line string, shard int, after string, limit int, kinds []string) (OutboxPage, error) {
	var out OutboxPage
	q := url.Values{"business_line": {line}, "shard": {strconv.Itoa(shard)}, "after": {after}, "limit": {strconv.Itoa(limit)}}
	if len(kinds) > 0 {
		q.Set("kinds", strings.Join(kinds, ","))
	}
	if err := c.request(WithBusinessLine(ctx, line), http.MethodGet, "/v1/outbox?"+q.Encode(), nil, &out); err != nil {
		return out, err
	}
	if out.Next == "" || out.Events == nil || len(out.Events) > limit {
		return OutboxPage{}, errors.New("kernel: invalid outbox page")
	}
	for _, e := range out.Events {
		if e.EnvelopeID == "" || e.BusinessLine != line || e.SessionID == "" {
			return OutboxPage{}, errors.New("kernel: invalid outbox identity")
		}
		switch e.Kind {
		case "session_deleted":
			if e.Seq != 0 || e.EndUser != "" {
				return OutboxPage{}, errors.New("kernel: invalid deletion envelope")
			}
		case "run_completed", "run_failed", "question", "approval_requested", "background_settled", "session_closed":
			if e.Seq == 0 || e.EndUser == "" {
				return OutboxPage{}, errors.New("kernel: invalid event envelope")
			}
		default:
			return OutboxPage{}, errors.New("kernel: unknown outbox kind")
		}
	}
	return out, nil
}

// StableSessions 只读一页，游标可随恢复进度落盘；关闭不影响枚举，404 由后续逐 ID 读取判定。
func (c *Client) StableSessions(ctx context.Context, line, user, after string, limit int) ([]SessionInfo, error) {
	q := url.Values{"business_line": {line}, "end_user": {user}, "order": {"session_id"}, "cursor": {after}, "limit": {strconv.Itoa(limit)}}
	var page struct {
		Sessions *[]SessionInfo `json:"sessions"`
	}
	if err := c.request(WithBusinessLine(ctx, line), http.MethodGet, "/v1/sessions?"+q.Encode(), nil, &page); err != nil {
		return nil, err
	}
	if page.Sessions == nil || len(*page.Sessions) > limit {
		return nil, errors.New("kernel: invalid stable session page")
	}
	for _, s := range *page.Sessions {
		if s.ID <= after || s.EndUser != user {
			return nil, errors.New("kernel: invalid stable session order or owner")
		}
		after = s.ID
	}
	return *page.Sessions, nil
}
