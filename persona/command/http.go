package command

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"persona/domain"
	"persona/mcpserver"
	"persona/query"
	"persona/store"
)

// Handler 仅接受与查询面相同的服务凭据；不复用开发夹具的无鉴权入口。
func Handler(writer Writer, credentials []mcpserver.Credential, now func() time.Time) http.Handler {
	credentials = append([]mcpserver.Credential(nil), credentials...)
	if now == nil {
		now = time.Now
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/commands/{endUser}/{operation}", func(w http.ResponseWriter, r *http.Request) {
		line := mcpserver.AuthenticateService(r, credentials, now())
		if mcpserver.ValidateCredentials(credentials) != nil || line == "" {
			query.WriteJSON(w, 401, map[string]string{"error": "unauthorized"})
			return
		}
		var in Request
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		d.DisallowUnknownFields()
		if r.URL.RawQuery != "" || d.Decode(&in) != nil || d.Decode(new(any)) != io.EOF {
			WriteError(w, domain.ErrUserInvalid)
			return
		}
		in.Operation = r.PathValue("operation")
		out, err := writer.Execute(r.Context(), store.Owner{BusinessLine: line, EndUser: r.PathValue("endUser")}, in)
		if err != nil {
			WriteError(w, err)
			return
		}
		query.WriteJSON(w, 200, out)
	})
	return mux
}

type Client struct {
	BaseURL string
	Token   func(context.Context, string) (string, error)
	HTTP    *http.Client
}

func (c *Client) Execute(ctx context.Context, owner store.Owner, in Request) (json.RawMessage, error) {
	unavailable := &query.Error{Status: 503, Code: "unavailable"}
	if owner.BusinessLine == "" || owner.EndUser == "" || c.Token == nil {
		return nil, unavailable
	}
	token, err := c.Token(ctx, owner.BusinessLine)
	if err != nil || token == "" {
		return nil, unavailable
	}
	data, err := json.Marshal(in)
	if err != nil {
		return nil, domain.ErrUserInvalid
	}
	path := strings.TrimRight(c.BaseURL, "/") + "/v1/commands/" + url.PathEscape(owner.EndUser) + "/" + url.PathEscape(in.Operation)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, path, bytes.NewReader(data))
	if err != nil {
		return nil, unavailable
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	transport := *client
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := transport.Do(req)
	if err != nil {
		return nil, unavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		code := map[int]string{400: "invalid_request", 403: "account_deleting", 404: "not_found", 409: "revision_conflict", 410: "gone"}[resp.StatusCode]
		if code == "" {
			return nil, unavailable
		}
		if resp.StatusCode == 409 || resp.StatusCode == 410 {
			var body struct {
				Error string `json:"error"`
			}
			if json.NewDecoder(io.LimitReader(resp.Body, 1024)).Decode(&body) == nil {
				if resp.StatusCode == 409 && body.Error == "deletion_pending" || resp.StatusCode == 410 && (body.Error == "expired" || body.Error == "deleted") {
					code = body.Error
				}
			}
		}
		return nil, &query.Error{Status: resp.StatusCode, Code: code}
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 || !json.Valid(data) {
		return nil, unavailable
	}
	return json.RawMessage(data), nil
}
