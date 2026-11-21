package k8s

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// Config 是访问 apiserver 所需的连接信息。只用到 Pod、pods/exec 与 NetworkPolicy 的少数接口，
// 所以直接走 REST，不引入 client-go。
type Config struct {
	// API 是 apiserver 地址，如 https://10.0.0.1:443。
	API string
	// TokenFile 是 ServiceAccount 令牌文件；每次请求重新读取，以跟上投射令牌的轮换。
	TokenFile string
	// CAFile 是 apiserver 的 CA 证书；为空时用系统根证书。
	CAFile string
	// Namespace 是沙箱 Pod 所在的命名空间（控制器的权限只在这里）。
	Namespace string
}

const saDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// InCluster 返回 Pod 内的默认连接信息（KUBERNETES_SERVICE_HOST 与挂载的 ServiceAccount）。
func InCluster(namespace string) (Config, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return Config{}, errors.New("k8s: not running in a cluster (KUBERNETES_SERVICE_HOST unset); pass the API address explicitly")
	}
	return Config{API: "https://" + strings.Trim(host, "[]") + ":" + port, TokenFile: saDir + "/token", CAFile: saDir + "/ca.crt", Namespace: namespace}, nil
}

// Client 是最小的 apiserver 客户端。
type Client struct {
	cfg  Config
	http *http.Client
}

func NewClient(cfg Config) (*Client, error) {
	if cfg.API == "" || cfg.Namespace == "" {
		return nil, errors.New("k8s: API and Namespace are required")
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("k8s: no certificates in %s", cfg.CAFile)
		}
		tlsCfg.RootCAs = pool
	}
	// 自定义 TLS 配置的 Transport 默认只走 HTTP/1.1，exec 的 WebSocket 升级需要它。
	tr := &http.Transport{TLSClientConfig: tlsCfg, Proxy: nil, MaxIdleConnsPerHost: 16, IdleConnTimeout: 90 * time.Second}
	return &Client{cfg: cfg, http: &http.Client{Transport: tr}}, nil
}

func (c *Client) token() (string, error) {
	if c.cfg.TokenFile == "" {
		return "", nil
	}
	b, err := os.ReadFile(c.cfg.TokenFile)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// apiError 是 apiserver 返回的非 2xx 响应。
type apiError struct {
	Code    int
	Reason  string
	Message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("k8s: %d %s: %s", e.Code, e.Reason, e.Message)
}

func isStatus(err error, code int) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Code == code
}

// do 发送一个 JSON 请求；out 非 nil 时解码响应。
func (c *Client) do(ctx context.Context, method, path, contentType string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.cfg.API, "/")+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		if contentType == "" {
			contentType = "application/json"
		}
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	tok, err := c.token()
	if err != nil {
		return err
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var st status
		_ = json.Unmarshal(data, &st)
		return &apiError{Code: resp.StatusCode, Reason: st.Reason, Message: st.Message}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *Client) nsPath(resource string) string {
	return "/api/v1/namespaces/" + url.PathEscape(c.cfg.Namespace) + "/" + resource
}

// status 是 metav1.Status 中用到的字段；exec 的错误通道也用它报告退出码。
type status struct {
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Details struct {
		Causes []struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"causes"`
	} `json:"details"`
}

// exec 的流通道（v5.channel.k8s.io）：每条二进制消息的首字节是通道号。
const (
	chStdin  = 0
	chStdout = 1
	chStderr = 2
	chError  = 3
	chClose  = 255 // v5：关闭某个通道（这里只用于结束 stdin）
)

const execProtocol = "v5.channel.k8s.io"

// exec 在 Pod 的 container 中执行 argv，返回退出码。stdin 为 nil 时不开启标准输入。
// 进程本身的失败（非零退出码）不是错误；连接、鉴权或容器不存在才返回错误。
func (c *Client) exec(ctx context.Context, pod, container string, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	q := url.Values{"container": {container}, "stdout": {"true"}, "stderr": {"true"}}
	if stdin != nil {
		q.Set("stdin", "true")
	}
	for _, a := range argv {
		q.Add("command", a)
	}
	u := strings.TrimSuffix(c.cfg.API, "/") + c.nsPath("pods/"+url.PathEscape(pod)+"/exec") + "?" + q.Encode()
	u = "ws" + strings.TrimPrefix(u, "http") // https → wss，http → ws
	h := http.Header{}
	tok, err := c.token()
	if err != nil {
		return 0, err
	}
	if tok != "" {
		h.Set("Authorization", "Bearer "+tok)
	}
	conn, resp, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPClient: c.http, HTTPHeader: h, Subprotocols: []string{execProtocol}})
	if err != nil {
		if resp != nil && resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusSwitchingProtocols {
			return 0, &apiError{Code: resp.StatusCode, Reason: "ExecUpgrade", Message: err.Error()}
		}
		return 0, fmt.Errorf("k8s exec: %w", err)
	}
	defer conn.CloseNow()
	if conn.Subprotocol() != execProtocol {
		return 0, fmt.Errorf("k8s exec: apiserver does not support %s (Kubernetes 1.30 or later is required)", execProtocol)
	}
	conn.SetReadLimit(64 << 20)

	stdinErr := make(chan error, 1)
	if stdin != nil {
		go func() {
			// 每帧 16KB：低于常见 WebSocket 实现的默认读取上限（32KB）。
			buf := make([]byte, 16<<10)
			frame := make([]byte, 1+len(buf))
			for {
				n, rerr := stdin.Read(buf)
				if n > 0 {
					frame[0] = chStdin
					copy(frame[1:], buf[:n])
					if err := conn.Write(ctx, websocket.MessageBinary, frame[:1+n]); err != nil {
						stdinErr <- err
						return
					}
				}
				if rerr == io.EOF {
					stdinErr <- conn.Write(ctx, websocket.MessageBinary, []byte{chClose, chStdin})
					return
				}
				if rerr != nil {
					stdinErr <- rerr
					return
				}
			}
		}()
	} else {
		stdinErr <- nil
	}

	var st *status
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) == websocket.StatusNormalClosure || errors.Is(err, io.EOF) {
				break
			}
			if st != nil { // 已收到结果，连接随后断开
				break
			}
			return 0, fmt.Errorf("k8s exec: %w", err)
		}
		if len(data) == 0 {
			continue
		}
		switch data[0] {
		case chStdout:
			if _, err := stdout.Write(data[1:]); err != nil {
				return 0, err
			}
		case chStderr:
			if _, err := stderr.Write(data[1:]); err != nil {
				return 0, err
			}
		case chError:
			st = &status{}
			if err := json.Unmarshal(data[1:], st); err != nil {
				return 0, fmt.Errorf("k8s exec: bad status: %w", err)
			}
		}
	}
	if err := <-stdinErr; err != nil && st == nil {
		return 0, fmt.Errorf("k8s exec stdin: %w", err)
	}
	return exitCode(st)
}

// exitCode 把 exec 错误通道上的 Status 转成退出码。
func exitCode(st *status) (int, error) {
	if st == nil {
		return 0, errors.New("k8s exec: stream ended without a status")
	}
	if st.Status == "Success" {
		return 0, nil
	}
	if st.Reason == "NonZeroExitCode" {
		for _, c := range st.Details.Causes {
			if c.Reason == "ExitCode" {
				n, err := strconv.Atoi(c.Message)
				if err != nil {
					return 0, fmt.Errorf("k8s exec: bad exit code %q", c.Message)
				}
				return n, nil
			}
		}
	}
	return 0, fmt.Errorf("k8s exec: %s: %s", st.Reason, st.Message)
}
