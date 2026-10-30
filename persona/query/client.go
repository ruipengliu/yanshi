package query

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"persona/store"
)

// RequestTimeout 包含正常的服务端排队与执行；连接建立另用短预算。
const RequestTimeout = 30 * time.Second

type Client struct {
	BaseURL string
	// Token 每请求按业务线取得服务凭据，允许部署方在轮换时替换秘密。
	Token func(context.Context, string) (string, error)
	HTTP  *http.Client
}

func urlValues(r *http.Request) (url.Values, error) {
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return nil, ErrInvalid
	}
	return values, nil
}

func (c *Client) Read(ctx context.Context, owner store.Owner, q Request) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	if err := q.validate(); err != nil {
		return nil, err
	}
	path := ""
	switch q.Resource {
	case "access", "projects", "commitments", "deletions":
		path = "/" + q.Resource
	case "project":
		path = "/projects/" + url.PathEscape(q.ID)
	case "exchanges":
		path = "/projects/" + url.PathEscape(q.ID) + "/exchanges"
	case "history":
		path = "/commitments/" + url.PathEscape(q.ID) + "/history"
	case "day", "captures":
		path = "/days/" + url.PathEscape(q.Date)
		if q.Resource == "captures" {
			path += "/captures"
		}
	case "week":
		path = "/weeks/" + url.PathEscape(q.Date)
	default:
		return nil, ErrInvalid
	}
	if owner.BusinessLine == "" || owner.EndUser == "" || c.Token == nil {
		return nil, &Error{Status: 503, Code: "unavailable"}
	}
	values := url.Values{}
	if q.Resource != "project" && q.Resource != "day" && q.Resource != "week" {
		values.Set("limit", strconv.Itoa(q.Limit))
		if q.Cursor != "" {
			values.Set("cursor", q.Cursor)
		}
	}
	if q.Resource == "commitments" {
		for k, v := range map[string]string{"kind": q.Filter.Kind, "status": q.Filter.Status, "project": q.Filter.Project, "counterpart": q.Filter.Counterpart, "query": q.Filter.Query, "due_from": q.Filter.DueFrom, "due_to": q.Filter.DueTo} {
			if v != "" {
				values.Set(k, v)
			}
		}
	}
	token, err := c.Token(ctx, owner.BusinessLine)
	if err != nil || token == "" {
		return nil, &Error{Status: 503, Code: "unavailable"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.BaseURL, "/")+"/v1/query/"+url.PathEscape(owner.EndUser)+path+"?"+values.Encode(), nil)
	if err != nil {
		return nil, &Error{Status: 503, Code: "unavailable"}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	transport := RequestClient(c.HTTP)
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := transport.Do(req)
	if err != nil {
		return nil, &Error{Status: 503, Code: "unavailable"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		// 只接受固定状态和分类，不把下游错误正文当可信内容透传。
		code := map[int]string{400: "invalid_query", 403: "account_deleting", 404: "not_found", 410: "gone"}[resp.StatusCode]
		if code == "" {
			return nil, &Error{Status: 503, Code: "unavailable"}
		}
		if resp.StatusCode == 410 {
			var body struct {
				Error string `json:"error"`
			}
			if json.NewDecoder(io.LimitReader(resp.Body, 1024)).Decode(&body) == nil && (body.Error == "expired" || body.Error == "deleted") {
				code = body.Error
			}
		}
		return nil, &Error{Status: resp.StatusCode, Code: code}
	}
	const maxResponse = 16 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil || len(data) > maxResponse || !json.Valid(data) {
		return nil, &Error{Status: 503, Code: "unavailable"}
	}
	return json.RawMessage(data), nil
}
