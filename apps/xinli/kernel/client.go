// Package kernel 只通过公开 HTTP 契约调用内核，不依赖内核 Go 类型。
package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func (c *Client) CreateSession(ctx context.Context, businessLine, endUser, agent string) (string, error) {
	in := struct {
		BusinessLine string `json:"business_line"`
		EndUser      string `json:"end_user"`
		Agent        string `json:"agent"`
	}{businessLine, endUser, agent}
	var out struct {
		SessionID string `json:"session_id"`
	}
	if err := c.post(ctx, "/v1/sessions", in, &out); err != nil {
		return "", err
	}
	if out.SessionID == "" {
		return "", errors.New("kernel: missing session_id")
	}
	return out.SessionID, nil
}

func (c *Client) Submit(ctx context.Context, sessionID, inputID, text string) error {
	in := struct {
		InputID string `json:"input_id"`
		Text    string `json:"text"`
	}{inputID, text}
	return c.post(ctx, "/v1/sessions/"+url.PathEscape(sessionID)+"/inputs", in, nil)
}

func (c *Client) post(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return errors.New("kernel: encode request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return errors.New("kernel: invalid request URL")
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	// API 不应重定向；保留原始非 2xx 状态，也避免把服务令牌发送到另一入口。
	transport := *client
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := transport.Do(req)
	// 提供商或代理可能把正文放进错误，不能把任意下游文字带进日志。
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("kernel: HTTP request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("kernel: HTTP status %d", resp.StatusCode)
	}
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
			return errors.New("kernel: invalid JSON response")
		}
	}
	return nil
}
