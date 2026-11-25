package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"yanshi/internal/extloop"
	"yanshi/internal/extloop/looptest"
)

type headerFlags map[string]string

func (h *headerFlags) String() string {
	if h == nil {
		return ""
	}
	parts := make([]string, 0, len(*h))
	for k, v := range *h {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, ",")
}

func (h *headerFlags) Set(s string) error {
	k, v, ok := strings.Cut(s, "=")
	k = strings.TrimSpace(k)
	if !ok || k == "" {
		return fmt.Errorf("want K=V, got %q", s)
	}
	if *h == nil {
		*h = map[string]string{}
	}
	(*h)[k] = v
	return nil
}

// looptestCmd 运行外部循环一致性套件（ADR-0062 第 8 条）：托管 OpenAI 兼容假模型，
// 经 HTTP 逐项校验被测外部循环服务并按场景输出结果。
func looptestCmd(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return runLooptest(ctx, args, os.Stdout)
}

func runLooptest(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("looptest", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	targetURL := fs.String("url", "", "被测外部循环服务的 HTTP 地址（必填）")
	modelAddr := fs.String("model-addr", "127.0.0.1:18471", "内置假模型（OpenAI 兼容）的监听地址")
	timeout := fs.Duration("timeout", extloop.DefaultTimeout, "每一步请求的时限")
	restartCmd := fs.String("restart", "", "可选：用于 restart 场景重启被测服务的 shell 命令")
	runFilter := fs.String("run", "", "可选：只运行指定场景（逗号分隔），可选值："+strings.Join(looptest.ScenarioNames(), ","))
	waitDur := fs.Duration("wait", 0, "可选：启动前等待 -url 可连接的最长时长（如 10s）")
	var headers headerFlags
	fs.Var(&headers, "header", "附加到每个请求的 HTTP 头（K=V，可重复）")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *targetURL == "" {
		return errors.New("-url is required")
	}
	u, err := url.Parse(*targetURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("-url %q: must be an http(s) URL", *targetURL)
	}

	fm := looptest.NewModel()
	ln, err := net.Listen("tcp", *modelAddr)
	if err != nil {
		return fmt.Errorf("listen fake model on %s: %w", *modelAddr, err)
	}
	modelSrv := &http.Server{Handler: fm, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = modelSrv.Serve(ln) }()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = modelSrv.Shutdown(sctx)
	}()

	fmt.Fprintf(stdout, "假模型地址：http://%s/v1\n", ln.Addr().String())
	fmt.Fprintln(stdout, "提示：被测服务须配置为调用它。")

	if *waitDur > 0 {
		if err := waitTCPReady(ctx, u, *waitDur); err != nil {
			return fmt.Errorf("wait for %s: %w", *targetURL, err)
		}
	}

	var restart func(context.Context) error
	if *restartCmd != "" {
		cmdStr := *restartCmd
		waitAfter := *waitDur
		restart = func(rctx context.Context) error {
			cmd := exec.CommandContext(rctx, "sh", "-c", cmdStr)
			out, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("command failed: %w (%s)", err, strings.TrimSpace(string(out)))
			}
			if waitAfter > 0 {
				if err := waitTCPReady(rctx, u, waitAfter); err != nil {
					return fmt.Errorf("service did not come back: %w", err)
				}
			}
			return nil
		}
	}

	var filter []string
	if *runFilter != "" {
		for _, s := range strings.Split(*runFilter, ",") {
			if s = strings.TrimSpace(s); s != "" {
				filter = append(filter, s)
			}
		}
	}

	results := looptest.Run(ctx, looptest.Config{
		URL:     *targetURL,
		Headers: headers,
		Timeout: *timeout,
		Model:   fm,
		Restart: restart,
		Run:     filter,
	})

	var passed, failed, skipped int
	for _, res := range results {
		switch res.Status {
		case looptest.StatusPass:
			passed++
		case looptest.StatusFail:
			failed++
		case looptest.StatusSkip:
			skipped++
		}
		if res.Reason != "" {
			fmt.Fprintf(stdout, "%-4s  %-22s  %s\n", res.Status, res.Name, res.Reason)
		} else {
			fmt.Fprintf(stdout, "%-4s  %-22s\n", res.Status, res.Name)
		}
	}
	fmt.Fprintf(stdout, "总计：%d 通过，%d 失败，%d 跳过\n", passed, failed, skipped)
	if failed > 0 {
		return fmt.Errorf("%d scenario(s) failed", failed)
	}
	return nil
}

// waitTCPReady 轮询目标 URL 的主机端口直到 TCP 可连接：不发 HTTP 请求，避免对只接受 POST 的循环端点产生噪音。
func waitTCPReady(ctx context.Context, u *url.URL, timeout time.Duration) error {
	addr := u.Host
	if _, _, err := net.SplitHostPort(addr); err != nil {
		if u.Scheme == "https" {
			addr = net.JoinHostPort(u.Hostname(), "443")
		} else {
			addr = net.JoinHostPort(u.Hostname(), "80")
		}
	}
	deadline := time.Now().Add(timeout)
	d := net.Dialer{Timeout: 500 * time.Millisecond}
	for {
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
