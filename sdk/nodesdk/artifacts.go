package nodesdk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	v1 "yanshi/gen/yanshi/v1"
)

// Artifacts 是设备侧的工件客户端（docs/design/m2-artifacts.md §3）。
type Artifacts struct {
	// BaseURL 是 yanshi API 地址，如 http://127.0.0.1:8080。
	BaseURL string
	Client  *http.Client
	// TokenSource 返回访问 API 的令牌，每次请求时调用；为空时不带令牌（开发模式）。
	TokenSource TokenSource
}

// TokenSource 返回当前有效的令牌。实现应缓存令牌并在到期前刷新。
type TokenSource func(ctx context.Context) (string, error)

func tokenOf(ctx context.Context, src TokenSource, fixed string) (string, error) {
	if src == nil {
		return fixed, nil
	}
	return src(ctx)
}

func (a *Artifacts) do(req *http.Request) (*http.Response, error) {
	token, err := tokenOf(req.Context(), a.TokenSource, "")
	if err != nil {
		return nil, fmt.Errorf("token: %w", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return a.client().Do(req)
}

// APIBaseFromGateway 由网关地址（ws://host/v1/nodes/connect）推导 API 地址。
func APIBaseFromGateway(gateway string) (string, error) {
	u, err := url.Parse(gateway)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	}
	u.Path, u.RawQuery = "", ""
	return strings.TrimRight(u.String(), "/"), nil
}

func (a *Artifacts) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}
	return http.DefaultClient
}

// Upload 上传文件为 sessionID 的工件，返回可放入调用结果的 Media 内容块。
func (a *Artifacts) Upload(ctx context.Context, sessionID, name, mimeType string, r io.Reader) (*v1.ContentBlock, error) {
	u := fmt.Sprintf("%s/v1/sessions/%s/artifacts?name=%s", a.BaseURL, url.PathEscape(sessionID), url.QueryEscape(name))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, r)
	if err != nil {
		return nil, err
	}
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	req.Header.Set("Content-Type", mimeType)
	resp, err := a.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("upload artifact: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var m struct {
		URI, Name string
		MimeType  string `json:"mime_type"`
		Size      uint64
	}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, err
	}
	return &v1.ContentBlock{Kind: &v1.ContentBlock_Media{Media: &v1.Media{Uri: m.URI, Name: m.Name, MimeType: m.MimeType, Size: m.Size}}}, nil
}

// Download 下载工件；id 可以是 "art_x" 或 "artifact://art_x"。返回的 sessionID 是工件所属 Session，
// 调用方应校验它与当前调用的 Session 一致。
func (a *Artifacts) Download(ctx context.Context, id string) (body io.ReadCloser, name, sessionID string, err error) {
	id = strings.TrimPrefix(id, "artifact://")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.BaseURL+"/v1/artifacts/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, "", "", err
	}
	resp, err := a.do(req)
	if err != nil {
		return nil, "", "", err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, "", "", fmt.Errorf("download artifact %s: %s", id, resp.Status)
	}
	name, _ = url.QueryUnescape(resp.Header.Get("X-Yanshi-Artifact-Name"))
	return resp.Body, name, resp.Header.Get("X-Yanshi-Session"), nil
}
