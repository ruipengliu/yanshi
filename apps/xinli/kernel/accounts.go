package kernel

import (
	"context"
	"errors"
	"net/http"
	"net/url"
)

type Tier struct {
	Name        string `json:"tier"`
	LimitMicros int64  `json:"limit_micros"`
}

func (c *Client) SetTier(ctx context.Context, bl, eu, tier string) (Tier, error) {
	ctx = WithBusinessLine(ctx, bl)
	path := "/v1/end_users/" + url.PathEscape(eu) + "/tier?" + url.Values{"business_line": {bl}}.Encode()
	method := http.MethodDelete
	var in any
	if tier != "" {
		method = http.MethodPut
		in = map[string]string{"tier": tier}
	}
	var out struct {
		Name        *string `json:"tier"`
		LimitMicros *int64  `json:"limit_micros"`
	}
	if err := c.request(ctx, method, path, in, &out); err != nil {
		return Tier{}, err
	}
	if out.Name == nil || out.LimitMicros == nil || *out.Name != tier {
		return Tier{}, errors.New("kernel: invalid tier response")
	}
	return Tier{Name: *out.Name, LimitMicros: *out.LimitMicros}, nil
}

func (c *Client) DeleteEndUser(ctx context.Context, bl, eu string) (string, error) {
	var out struct {
		RequestID string `json:"request_id"`
	}
	err := c.request(WithBusinessLine(ctx, bl), http.MethodDelete, "/v1/end_users/"+url.PathEscape(eu)+"?"+url.Values{"business_line": {bl}}.Encode(), nil, &out)
	if err == nil && out.RequestID == "" {
		err = errors.New("kernel: missing deletion request_id")
	}
	return out.RequestID, err
}

type EndUserDeletion struct {
	RequestID string `json:"request_id"`
	Sessions  int    `json:"sessions"`
	Completed int    `json:"completed"`
	Done      bool   `json:"done"`
}

func (c *Client) EndUserDeletion(ctx context.Context, bl, id string) (EndUserDeletion, error) {
	// 指针字段防止缺字段被当成零 Session 的完成证明。
	var out struct {
		RequestID string `json:"request_id"`
		Sessions  *int   `json:"sessions"`
		Completed *int   `json:"completed"`
		Done      *bool  `json:"done"`
	}
	err := c.request(WithBusinessLine(ctx, bl), http.MethodGet, "/v1/deletions/"+url.PathEscape(id)+"?"+url.Values{"business_line": {bl}}.Encode(), nil, &out)
	if err != nil {
		return EndUserDeletion{}, err
	}
	if out.RequestID != id || out.Sessions == nil || out.Completed == nil || out.Done == nil || *out.Sessions < 0 || *out.Completed < 0 || *out.Completed > *out.Sessions || *out.Done != (*out.Completed == *out.Sessions) {
		return EndUserDeletion{}, errors.New("kernel: invalid deletion progress")
	}
	return EndUserDeletion{id, *out.Sessions, *out.Completed, *out.Done}, nil
}
