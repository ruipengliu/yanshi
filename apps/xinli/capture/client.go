package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
)

var ErrResync = errors.New("capture requires explicit resynchronization")
var ErrRejected = errors.New("capture permanently rejected")

// Client 每次发送取得当前应用凭据；不会保存或使用内核令牌。
type Client struct {
	BaseURL string
	HTTP    *http.Client
	Token   func(context.Context) (string, error)
}

func (c Client) Send(ctx context.Context, b Batch) (Receipt, error) {
	raw, err := json.Marshal(b)
	if err != nil {
		return Receipt{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/v1/captures/batches", bytes.NewReader(raw))
	if err != nil {
		return Receipt{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return Receipt{}, err
	}
	defer resp.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted {
		var out Receipt
		if err := decoder.Decode(&out); err != nil {
			return Receipt{}, err
		}
		if resp.StatusCode == http.StatusOK && out.Status != "accepted" || resp.StatusCode == http.StatusAccepted && out.Status != "staged" {
			return Receipt{}, errors.New("invalid capture response")
		}
		return out, nil
	}
	var out Error
	_ = decoder.Decode(&out)
	if resp.StatusCode == http.StatusConflict && out.Code == "resync_required" {
		return out.Receipt, ErrResync
	}
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusForbidden {
		return out.Receipt, ErrRejected
	}
	return Receipt{}, errors.New("capture request unavailable")
}

func (c Client) Sync(ctx context.Context, day, session string) (Receipt, error) {
	values := url.Values{"business_day": {day}, "session_id": {session}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.BaseURL, "/")+"/v1/captures/sync?"+values.Encode(), nil)
	if err != nil {
		return Receipt{}, err
	}
	response, err := c.do(req)
	if err != nil {
		return Receipt{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Receipt{}, ErrRejected
	}
	var out Receipt
	err = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&out)
	return out, err
}

func (c Client) CloseSession(ctx context.Context, session string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/v1/captures/sessions/"+url.PathEscape(session)+"/close", nil)
	if err != nil {
		return err
	}
	response, err := c.do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return ErrRejected
	}
	return nil
}

func (c Client) do(req *http.Request) (*http.Response, error) {
	if c.Token == nil {
		return nil, ErrRejected
	}
	token, err := c.Token(req.Context())
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	return client.Do(req)
}
