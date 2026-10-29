package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// ThreadRequest 的路径由 BFF 的固定路由构造，不能接收客户端自报的上游 URL。
func (c *Client) ThreadRequest(ctx context.Context, line, id, method, suffix string, in any) (json.RawMessage, error) {
	var out json.RawMessage
	var result any = &out
	if method == http.MethodDelete || strings.HasPrefix(suffix, "/answers/") || strings.HasPrefix(suffix, "/approvals/") || strings.HasSuffix(suffix, "/interrupt") {
		result = nil
	}
	err := c.requestJSON(WithBusinessLine(ctx, line), method, sessionPath(id)+suffix, in, result, 16<<20)
	return out, err
}

func (c *Client) DeleteMemory(ctx context.Context, line, user, id string) error {
	return c.request(WithBusinessLine(ctx, line), http.MethodDelete, "/v1/memories/"+url.PathEscape(id)+"?"+url.Values{"business_line": {line}, "end_user": {user}}.Encode(), nil, nil)
}

// Stream 不设置整个响应的时限：内核在服务令牌到期时结束流，下一次请求再向 TokenSource 取令牌。
// 调用方负责关闭 Body；ctx 取消同时撤销尚在等待响应头的请求与正在读取的流。
func (c *Client) Stream(ctx context.Context, line, id string, after uint64) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.BaseURL, "/")+sessionPath(id)+"/stream?after="+strconv.FormatUint(after, 10), nil)
	if err != nil || c.Tokens == nil {
		return nil, errToken
	}
	token, err := c.Tokens.Token(ctx, line)
	if err != nil || token == "" {
		return nil, errToken
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	transport := *client
	transport.Timeout = 0
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := transport.Do(req)
	if err != nil {
		return nil, errors.New("kernel: stream unavailable")
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, responseError(resp)
	}
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || media != "text/event-stream" {
		_ = resp.Body.Close()
		return nil, errors.New("kernel: invalid stream response")
	}
	return resp.Body, nil
}

// 只提取重答与出站恢复的固定分类，不让任意下游正文进入错误或日志。
func responseError(resp *http.Response) error {
	err := &HTTPError{StatusCode: resp.StatusCode}
	if resp.StatusCode != http.StatusConflict && resp.StatusCode != http.StatusGone {
		return err
	}
	var body struct {
		Error string `json:"error"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 2048)).Decode(&body) != nil {
		return err
	}
	if resp.StatusCode == http.StatusGone {
		if body.Error == "outbox_cursor_expired" || body.Error == "outbox_epoch_changed" {
			err.Reason = body.Error
		}
		return err
	}
	for _, reason := range []string{"session_closed", "not_last_run", "not_completed", "delegated_session", "platform_input", "card_action_input", "compacted", "steered", "approval_requested", "ask_user", "delegation", "background_call", "not_read_only"} {
		if body.Error == "conflict: redo: "+reason {
			err.Reason = reason
			break
		}
	}
	return err
}
