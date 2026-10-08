// Package docker 是基于 Docker CLI 的 sandbox.Provider，用于开发环境（ADR-0010：生产为 Kubernetes + gVisor）。
//
// 安全配置：默认无网络、丢弃全部 capability、no-new-privileges、非 root、只读根文件系统、
// 限制 CPU/内存/进程数；可选 gVisor 运行时（Runtime: "runsc"）。
// 工作区是命名卷，容器删除后保留。
package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"yanshi/internal/sandbox"
)

type Provider struct {
	Image string
	// Runtime 非空时传给 --runtime，如 "runsc"（gVisor）。
	Runtime  string
	CPUs     float64
	MemoryMB int
	PIDs     int
	// Network 为空时无网络（--network none）。
	Network string
	// UID 是沙箱内运行代码的用户。
	UID int
}

func (p *Provider) defaults() {
	if p.CPUs == 0 {
		p.CPUs = 1
	}
	if p.MemoryMB == 0 {
		p.MemoryMB = 1024
	}
	if p.PIDs == 0 {
		p.PIDs = 256
	}
	if p.Network == "" {
		p.Network = "none"
	}
	if p.UID == 0 {
		p.UID = 1000
	}
}

var _ sandbox.Provider = (*Provider)(nil)

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)

func container(id string) string { return "yanshi-sbx-" + unsafeName.ReplaceAllString(id, "-") }
func volume(id string) string    { return "yanshi-ws-" + unsafeName.ReplaceAllString(id, "-") }

// docker 运行 docker CLI，返回 stdout；失败时错误包含 stderr。
func docker(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

func (p *Provider) Ensure(ctx context.Context, id string) error {
	p.defaults()
	name := container(id)
	out, err := docker(ctx, nil, "inspect", "-f", "{{.State.Running}}", name)
	if err == nil {
		if strings.TrimSpace(string(out)) == "true" {
			return nil
		}
		_, err := docker(ctx, nil, "start", name)
		return err
	}
	if _, err := docker(ctx, nil, "volume", "inspect", volume(id)); err != nil {
		if _, err := docker(ctx, nil, "volume", "create", "--label", "yanshi.sandbox="+id, volume(id)); err != nil {
			return err
		}
		// 新卷归 root 所有：以最小权限一次性把属主改为沙箱用户。
		uid := strconv.Itoa(p.UID)
		if _, err := docker(ctx, nil, "run", "--rm", "--network", "none", "--cap-drop", "ALL", "--cap-add", "CHOWN",
			"-v", volume(id)+":"+sandbox.Workspace, p.Image, "chown", uid+":"+uid, sandbox.Workspace); err != nil {
			return err
		}
	}
	args := []string{"run", "-d", "--name", name,
		"--label", "yanshi.sandbox=" + id,
		"--network", p.Network,
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--user", fmt.Sprintf("%d:%d", p.UID, p.UID),
		"--read-only", "--tmpfs", "/tmp:rw,size=256m",
		"--cpus", strconv.FormatFloat(p.CPUs, 'f', -1, 64),
		"--memory", fmt.Sprintf("%dm", p.MemoryMB),
		"--pids-limit", strconv.Itoa(p.PIDs),
		"-e", "HOME=" + sandbox.Workspace, "-e", "MPLCONFIGDIR=/tmp",
		// 不传 -w：Docker 创建工作目录时会把卷挂载点属主重置为 root。工作目录由每次 exec 指定。
		"-v", volume(id) + ":" + sandbox.Workspace,
	}
	if p.Runtime != "" {
		args = append(args, "--runtime", p.Runtime)
	}
	args = append(args, p.Image, "sleep", "infinity")
	if _, err := docker(ctx, nil, args...); err != nil {
		// 并发创建：名称已被占用说明另一方已创建，确保其运行即可。
		if strings.Contains(err.Error(), "already in use") {
			_, err = docker(ctx, nil, "start", name)
		}
		return err
	}
	return nil
}

func (p *Provider) Exec(ctx context.Context, id string, req sandbox.ExecRequest) (*sandbox.ExecResult, error) {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = sandbox.DefaultExecTimeout
	}
	secs := strconv.Itoa(int(timeout.Seconds()))
	// 超时在沙箱内由 timeout 强制执行：即使控制器断开，进程也不会无限运行。
	args := append([]string{"exec", "-i", "-w", sandbox.Workspace, container(id), "timeout", "-s", "KILL", secs}, req.Argv...)
	// 客户端额外留出余量，以便正常拿到沙箱内超时的退出码。
	cctx, cancel := context.WithTimeout(ctx, timeout+10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "docker", args...)
	stdout := &sandbox.LimitedBuffer{Max: sandbox.OutputLimit}
	stderr := &sandbox.LimitedBuffer{Max: sandbox.OutputLimit}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if req.Stdin != nil {
		cmd.Stdin = bytes.NewReader(req.Stdin)
	}
	start := time.Now()
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	res := &sandbox.ExecResult{
		Stdout: stdout.Bytes(), Stderr: stderr.Bytes(),
		StdoutTruncated: stdout.Truncated, StderrTruncated: stderr.Truncated,
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		res.ExitCode = exitErr.ExitCode()
		// docker exec 自身的错误（容器不存在等）退出码为 125–127 且输出在 stderr，交由调用方重试。
		if res.ExitCode == 125 || (res.ExitCode == 126 && bytes.Contains(res.Stderr, []byte("OCI runtime"))) {
			return nil, fmt.Errorf("docker exec: %s", bytes.TrimSpace(res.Stderr))
		}
		res.TimedOut = res.ExitCode == 137 && time.Since(start) >= timeout-time.Second
	default:
		return nil, err
	}
	return res, nil
}

func (p *Provider) WriteFile(ctx context.Context, id, path string, data []byte) error {
	_, err := docker(ctx, data, "exec", "-i", container(id), "sh", "-c", `mkdir -p "$(dirname "$1")" && cat > "$1"`, "sh", path)
	return err
}

func (p *Provider) ReadFile(ctx context.Context, id, path string, max int) ([]byte, bool, error) {
	out, err := docker(ctx, nil, "exec", container(id), "head", "-c", strconv.Itoa(max+1), path)
	if err != nil {
		return nil, false, err
	}
	if len(out) > max {
		return out[:max], true, nil
	}
	return out, false, nil
}

func (p *Provider) Stop(ctx context.Context, id string) error {
	_, err := docker(ctx, nil, "rm", "-f", container(id))
	return err
}

func (p *Provider) Destroy(ctx context.Context, id string) error {
	if err := p.Stop(ctx, id); err != nil {
		return err
	}
	_, err := docker(ctx, nil, "volume", "rm", "-f", volume(id))
	return err
}
