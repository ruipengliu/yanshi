package xinlifixture

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"yanshi/internal/mcpcap"
)

// Client 持有进程和 stdin；关闭管道让夹具正常退出，取消或超时也必须回收进程。
type Client struct {
	base     string
	http     *http.Client
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	done     chan struct{}
	once     sync.Once
	waitErr  error
	closeErr error
}

func Start(ctx context.Context, bin string, businessLines ...string) (*Client, []mcpcap.Server, error) {
	cmd := exec.Command(bin, "dev", "-business-lines", strings.Join(businessLines, ","), "-exit-on-stdin-eof")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	// 自管 stdout 管道，让 Wait 回收进程时不会提前关闭尚未读完的就绪行。
	stdout, output, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		return nil, nil, err
	}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = output.Close()
		return nil, nil, fmt.Errorf("start xinli fixture %q: %w; make eval builds bin/xinli (or set -xinli-bin)", bin, err)
	}
	_ = output.Close()
	c := &Client{cmd: cmd, stdin: stdin, done: make(chan struct{}), http: &http.Client{Timeout: 30 * time.Second}}
	go func() {
		c.waitErr = cmd.Wait()
		close(c.done)
	}()
	type announcement struct {
		FixtureURL    string            `json:"fixture_url"`
		Registrations []json.RawMessage `json:"registrations"`
	}
	type readyResult struct {
		ready announcement
		err   error
	}
	ready := make(chan readyResult, 1)
	go func() {
		defer stdout.Close()
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 4096), 1<<20)
		var result readyResult
		if scanner.Scan() {
			result.err = json.Unmarshal(scanner.Bytes(), &result.ready)
		} else {
			result.err = scanner.Err()
			if result.err == nil {
				result.err = io.ErrUnexpectedEOF
			}
		}
		ready <- result
		// stdout 只承载就绪协议；继续排空，避免意外输出堵住子进程。
		_, _ = io.Copy(io.Discard, stdout)
	}()
	timeout := time.NewTimer(15 * time.Second)
	defer timeout.Stop()
	var result readyResult
	select {
	case result = <-ready:
	case <-ctx.Done():
		result.err = ctx.Err()
	case <-timeout.C:
		result.err = fmt.Errorf("readiness timed out")
	}
	fail := func(err error) (*Client, []mcpcap.Server, error) {
		_ = c.Close()
		return nil, nil, fmt.Errorf("xinli fixture readiness: %w", err)
	}
	if result.err != nil {
		return fail(result.err)
	}
	u, err := url.Parse(result.ready.FixtureURL)
	if err != nil || u.Scheme != "http" || u.Host == "" {
		return fail(fmt.Errorf("invalid fixture_url"))
	}
	c.base = strings.TrimRight(result.ready.FixtureURL, "/")
	if len(result.ready.Registrations) == 0 {
		return fail(fmt.Errorf("no MCP registrations"))
	}
	var servers []mcpcap.Server
	for _, raw := range result.ready.Registrations {
		server, err := mcpcap.ParseServer(raw)
		if err != nil {
			return fail(err)
		}
		servers = append(servers, server)
	}
	go func() {
		select {
		case <-ctx.Done():
			_ = c.Close()
		case <-c.done:
		}
	}()
	return c, servers, nil
}

// Close 可重复调用；Wait 只有一个调用者，超时 Kill 后仍等待回收。
func (c *Client) Close() error {
	c.once.Do(func() {
		_ = c.stdin.Close()
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case <-c.done:
		case <-timer.C:
			_ = c.cmd.Process.Kill()
			<-c.done
		}
		c.closeErr = c.waitErr
		c.http.CloseIdleConnections()
	})
	return c.closeErr
}

func (c *Client) request(ctx context.Context, bl, eu, op string, input, output any) error {
	var body io.Reader
	method := http.MethodGet
	if input != nil {
		raw, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body, method = bytes.NewReader(raw), http.MethodPost
	}
	endpoint := c.base + "/v1/fixture/" + url.PathEscape(bl) + "/" + url.PathEscape(eu) + "/" + op
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("xinli fixture %s: %w", op, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var value struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&value)
		return fmt.Errorf("xinli fixture %s: HTTP %d: %.240s", op, resp.StatusCode, value.Error)
	}
	if output != nil {
		return json.NewDecoder(resp.Body).Decode(output)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	return err
}

func (c *Client) Seed(ctx context.Context, bl, eu string, seed SeedData) error {
	return c.request(ctx, bl, eu, "seed", seed, nil)
}

func (c *Client) State(ctx context.Context, bl, eu string) (State, error) {
	var state State
	err := c.request(ctx, bl, eu, "state", nil, &state)
	return state, err
}

func (c *Client) Bind(ctx context.Context, bl, eu, project, session string) error {
	return c.request(ctx, bl, eu, "bind", map[string]string{"project": project, "session": session}, nil)
}

func (c *Client) Submitted(ctx context.Context, bl, eu, session, input string) error {
	return c.request(ctx, bl, eu, "submitted", map[string]string{"session": session, "input": input}, nil)
}

func (c *Client) Edit(ctx context.Context, bl, eu, id, status, due string) error {
	return c.request(ctx, bl, eu, "edit", map[string]string{"id": id, "status": status, "due_date": due}, nil)
}

// CaptureSegment 是夹具的线上输入，不依赖 Persona 的 Go 模块。
type CaptureSegment struct {
	Speaker    string `json:"speaker"`
	SpeakerKey string `json:"speaker_key,omitempty"`
	Text       string `json:"text"`
	SpokenAt   string `json:"spoken_at,omitempty"`
	TimeZone   string `json:"time_zone,omitempty"`
}

func (c *Client) AppendCaptures(ctx context.Context, bl, eu, date, captureID string, segments []CaptureSegment) ([]string, error) {
	in := struct {
		Date      string           `json:"date"`
		CaptureID string           `json:"capture_id"`
		Segments  []CaptureSegment `json:"segments"`
	}{date, captureID, segments}
	var out struct {
		Refs []string `json:"refs"`
	}
	err := c.request(ctx, bl, eu, "captures", in, &out)
	return out.Refs, err
}

func (c *Client) BindDay(ctx context.Context, bl, eu, date, session string) error {
	return c.request(ctx, bl, eu, "bind_day", map[string]string{"date": date, "session": session}, nil)
}
