package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// SessionView 只读公开投影；服务令牌没有 EndUser 约束，调用方必须核验这里的 owner。
type SessionView struct {
	ID           string `json:"session_id"`
	BusinessLine string `json:"business_line"`
	EndUser      string `json:"end_user"`
	Agent        struct {
		Name string `json:"name"`
	} `json:"agent"`
}

func (c *Client) SessionView(ctx context.Context, line, id string) (SessionView, error) {
	var out SessionView
	err := c.request(WithBusinessLine(ctx, line), http.MethodGet, sessionPath(id), nil, &out)
	if err == nil && (out.ID != id || out.BusinessLine != line || out.EndUser == "" || out.Agent.Name == "") {
		err = errors.New("kernel: invalid session response")
	}
	return out, err
}

type EventPage struct {
	Events []json.RawMessage
	Last   uint64
}

func (c *Client) Events(ctx context.Context, line, id string, after uint64, limit int) (EventPage, error) {
	var out struct {
		Events *[]json.RawMessage `json:"events"`
	}
	values := url.Values{"after": {strconv.FormatUint(after, 10)}, "limit": {strconv.Itoa(limit)}}
	err := c.requestJSON(WithBusinessLine(ctx, line), http.MethodGet, sessionPath(id)+"/events?"+values.Encode(), nil, &out, 16<<20)
	if err != nil {
		return EventPage{}, err
	}
	if out.Events == nil || len(*out.Events) > limit {
		return EventPage{}, errors.New("kernel: invalid events response")
	}
	page := EventPage{Events: *out.Events, Last: after}
	for _, raw := range page.Events {
		var event struct {
			Session string      `json:"session_id"`
			Seq     json.Number `json:"seq"`
		}
		if json.Unmarshal(raw, &event) != nil {
			return EventPage{}, errors.New("kernel: invalid event")
		}
		n, err := strconv.ParseUint(string(event.Seq), 10, 64)
		if err != nil || n <= page.Last || event.Session != id {
			return EventPage{}, errors.New("kernel: invalid event sequence")
		}
		page.Last = n
	}
	return page, nil
}

type QuotaPeriod struct {
	Tier         string    `json:"tier,omitempty"`
	Scope        string    `json:"scope"`
	LimitMicros  int64     `json:"limit_micros"`
	SpentMicros  int64     `json:"spent_micros"`
	Start        time.Time `json:"start"`
	ResetsAt     time.Time `json:"resets_at"`
	Exceeded     bool      `json:"exceeded"`
	BusinessLine string    `json:"business_line"`
	EndUser      string    `json:"end_user,omitempty"`
}

func (c *Client) Quota(ctx context.Context, line, user string) ([]QuotaPeriod, error) {
	var out struct {
		Periods *[]QuotaPeriod `json:"periods"`
	}
	values := url.Values{"business_line": {line}, "end_user": {user}}
	if err := c.request(WithBusinessLine(ctx, line), http.MethodGet, "/v1/quota?"+values.Encode(), nil, &out); err != nil {
		return nil, err
	}
	if out.Periods == nil {
		return nil, errors.New("kernel: missing quota periods")
	}
	periods := []QuotaPeriod{}
	for _, p := range *out.Periods {
		if p.Scope == "business_line" {
			continue // 业务线总用量包含其他人的活动，不能交给产品客户端。
		}
		if p.Scope != "end_user" || p.BusinessLine != line || p.EndUser != user {
			return nil, errors.New("kernel: invalid quota owner")
		}
		periods = append(periods, p)
	}
	return periods, nil
}
