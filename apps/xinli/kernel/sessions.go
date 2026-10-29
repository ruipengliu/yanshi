package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"time"
)

type SessionInfo struct {
	ID        string    `json:"session_id"`
	EndUser   string    `json:"end_user"`
	Agent     string    `json:"agent"`
	CreatedAt time.Time `json:"created_at"`
}

// Sessions 完整翻页；列表按最近输入排序，不能用创建时间提前截断。
func (c *Client) Sessions(ctx context.Context, bl, eu string) ([]SessionInfo, error) {
	var out []SessionInfo
	cursor := ""
	seen := map[string]bool{}
	for {
		query := url.Values{"business_line": {bl}, "end_user": {eu}, "limit": {"200"}, "cursor": {cursor}}
		var page struct {
			Sessions *[]SessionInfo `json:"sessions"`
			Next     string         `json:"next_cursor"`
		}
		if err := c.requestJSON(ctx, http.MethodGet, "/v1/sessions?"+query.Encode(), nil, &page, 0); err != nil {
			return nil, err
		}
		if page.Sessions == nil {
			return nil, errors.New("kernel: missing sessions")
		}
		for _, s := range *page.Sessions {
			if s.ID == "" || s.EndUser != eu || s.CreatedAt.IsZero() {
				return nil, errors.New("kernel: invalid session list")
			}
			out = append(out, s)
		}
		if page.Next == "" {
			return out, nil
		}
		if seen[page.Next] {
			return nil, errors.New("kernel: invalid session cursor")
		}
		seen[page.Next], cursor = true, page.Next
	}
}

// EmptySession 只接受唯一的创建事件；任何输入、定时或其他事件都阻止空孤儿删除。
func (c *Client) EmptySession(ctx context.Context, id string) (bool, error) {
	var page struct {
		Events *[]struct {
			Session string          `json:"session_id"`
			Seq     json.Number     `json:"seq"`
			Created json.RawMessage `json:"session_created"`
		} `json:"events"`
	}
	if err := c.requestJSON(ctx, http.MethodGet, sessionPath(id)+"/events?after=0&limit=2", nil, &page, 0); err != nil {
		return false, err
	}
	if page.Events == nil {
		return false, errors.New("kernel: missing events")
	}
	if len(*page.Events) != 1 {
		return false, nil
	}
	e := (*page.Events)[0]
	return e.Session == id && string(e.Seq) == "1" && len(e.Created) > 0 && string(e.Created) != "null", nil
}
