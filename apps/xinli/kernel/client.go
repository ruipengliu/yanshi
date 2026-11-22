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
	Tokens  TokenSource
	HTTP    *http.Client
}

func (c *Client) CreateSession(ctx context.Context, businessLine, endUser, agent string) (string, error) {
	return c.CreateSessionWithOptions(ctx, businessLine, endUser, agent, SessionOptions{})
}

type SessionOptions struct {
	TimeZone       string
	DeleteAt       time.Time
	IdempotencyKey string
}

func (c *Client) CreateSessionWithOptions(ctx context.Context, businessLine, endUser, agent string, opts SessionOptions) (string, error) {
	ctx = WithBusinessLine(ctx, businessLine)
	in := struct {
		TimeZone       string     `json:"time_zone,omitempty"`
		BusinessLine   string     `json:"business_line"`
		EndUser        string     `json:"end_user"`
		Agent          string     `json:"agent"`
		DeleteAt       *time.Time `json:"delete_at,omitempty"`
		IdempotencyKey string     `json:"idempotency_key,omitempty"`
	}{BusinessLine: businessLine, EndUser: endUser, Agent: agent, TimeZone: opts.TimeZone, IdempotencyKey: opts.IdempotencyKey}
	if !opts.DeleteAt.IsZero() {
		in.DeleteAt = &opts.DeleteAt
	}
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
	return c.request(ctx, http.MethodPost, path, in, out)
}

func (c *Client) request(ctx context.Context, method, path string, in, out any) error {
	return c.requestJSON(ctx, method, path, in, out, 1<<20)
}

func (c *Client) requestJSON(ctx context.Context, method, path string, in, out any, maxBytes int64) error {
	var body []byte
	var err error
	if in != nil {
		body, err = json.Marshal(in)
	}
	if err != nil {
		return errors.New("kernel: encode request")
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return errors.New("kernel: invalid request URL")
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Tokens != nil {
		line, _ := ctx.Value(businessLineKey{}).(string)
		if line == "" {
			return errToken
		}
		token, err := c.Tokens.Token(ctx, line)
		if err != nil || token == "" {
			return errToken
		}
		req.Header.Set("Authorization", "Bearer "+token)
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
		return responseError(resp)
	}
	if out != nil {
		var body io.Reader = resp.Body
		if maxBytes > 0 {
			body = io.LimitReader(body, maxBytes)
		}
		if err := json.NewDecoder(body).Decode(out); err != nil {
			return errors.New("kernel: invalid JSON response")
		}
	}
	return nil
}

// HTTPError 只保留状态码与白名单分类，不保存响应正文。
type HTTPError struct {
	StatusCode int
	Reason     string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("kernel: HTTP status %d", e.StatusCode) }
func (e *HTTPError) Is(target error) bool {
	return target == ErrNotFound && e.StatusCode == http.StatusNotFound
}

var ErrNotFound = errors.New("kernel: not found")

type Session struct {
	Runs []Run `json:"runs"`
}
type Run struct {
	ID     string `json:"run_id"`
	Status string `json:"status"`
}

func (s Session) ActiveRun() string {
	for _, run := range s.Runs {
		if run.Status == "queued" || run.Status == "running" || run.Status == "suspended" {
			return run.ID
		}
	}
	return ""
}

type Deletion struct {
	Reason      string    `json:"reason"`
	RequestedAt time.Time `json:"requested_at"`
	CompletedAt time.Time `json:"completed_at"`
}
type Memory struct {
	ID      string `json:"id"`
	Content string `json:"content"`
}

func sessionPath(id string) string { return "/v1/sessions/" + url.PathEscape(id) }
func (c *Client) Session(ctx context.Context, id string) (Session, error) {
	var out Session
	err := c.request(ctx, http.MethodGet, sessionPath(id), nil, &out)
	return out, err
}
func (c *Client) Interrupt(ctx context.Context, sessionID, runID string) error {
	return c.request(ctx, http.MethodPost, sessionPath(sessionID)+"/runs/"+url.PathEscape(runID)+"/interrupt", nil, nil)
}
func (c *Client) DeleteSession(ctx context.Context, id, reason string) error {
	path := sessionPath(id)
	if reason != "" {
		path += "?" + url.Values{"reason": {reason}}.Encode()
	}
	return c.request(ctx, http.MethodDelete, path, nil, nil)
}
func (c *Client) Deletion(ctx context.Context, businessLine, id string) (Deletion, error) {
	ctx = WithBusinessLine(ctx, businessLine)
	var out Deletion
	err := c.request(ctx, http.MethodGet, sessionPath(id)+"/deletion?"+url.Values{"business_line": {businessLine}}.Encode(), nil, &out)
	return out, err
}
func (c *Client) DeleteSessionMemories(ctx context.Context, businessLine, id string) error {
	ctx = WithBusinessLine(ctx, businessLine)
	return c.request(ctx, http.MethodDelete, sessionPath(id)+"/memories?"+url.Values{"business_line": {businessLine}}.Encode(), nil, nil)
}
func (c *Client) MemoriesBySource(ctx context.Context, businessLine, endUser string, sessions []string) ([]Memory, error) {
	ctx = WithBusinessLine(ctx, businessLine)
	out := []Memory{}
	seen := map[string]bool{}
	for start := 0; start < len(sessions); start += 32 {
		query := url.Values{"business_line": {businessLine}, "end_user": {endUser}}
		for _, id := range sessions[start:min(start+32, len(sessions))] {
			query.Add("source_session", id)
		}
		var batch struct {
			Memories []Memory `json:"memories"`
		}
		if err := c.request(ctx, http.MethodGet, "/v1/memories?"+query.Encode(), nil, &batch); err != nil {
			return nil, err
		}
		for _, m := range batch.Memories {
			if !seen[m.ID] {
				seen[m.ID] = true
				out = append(out, m)
			}
		}
	}
	return out, nil
}

// OneshotResult 保留 output 的 JSON 形状，声明可返回字符串或结构化值。
type OneshotResult struct {
	Version string          `json:"version"`
	Model   string          `json:"model"`
	Output  json.RawMessage `json:"output"`
}

func (c *Client) Oneshot(ctx context.Context, endUser, name, input string) (OneshotResult, error) {
	return c.OneshotForBusinessLine(ctx, "", endUser, name, input)
}

// OneshotForBusinessLine 让未开启鉴权的开发入口也能显式指定业务线；服务令牌仍由内核核验。
func (c *Client) OneshotForBusinessLine(ctx context.Context, businessLine, endUser, name, input string) (OneshotResult, error) {
	if businessLine != "" {
		ctx = WithBusinessLine(ctx, businessLine)
	}
	in := struct {
		BusinessLine string `json:"business_line,omitempty"`
		EndUser      string `json:"end_user"`
		Input        string `json:"input"`
	}{businessLine, endUser, input}
	var out OneshotResult
	err := c.post(ctx, "/v1/oneshots/"+url.PathEscape(name), in, &out)
	return out, err
}
