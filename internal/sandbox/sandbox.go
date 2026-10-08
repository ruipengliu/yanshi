// Package sandbox 实现云端代码沙箱：每个 Session 一个，以虚拟 Node 的身份接入（ADR-0008、0009）。
// 语义见 docs/design/m2-sandbox.md。
package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"strings"
	"time"
)

// NodePrefix 是沙箱虚拟 Node ID 的前缀。
const NodePrefix = "sbx_"

// NodeID 返回 Session 的沙箱 Node ID。
func NodeID(sessionID string) string { return NodePrefix + sessionID }

// IsNode 判断 Node ID 是否属于沙箱。
func IsNode(nodeID string) bool { return strings.HasPrefix(nodeID, NodePrefix) }

// Workspace 是沙箱内工作区的挂载点。
const Workspace = "/workspace"

// WorkspacePath 把相对路径解析为工作区内的绝对路径，拒绝越界。
func WorkspacePath(rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("path is required")
	}
	p := path.Clean(path.Join(Workspace, rel))
	if strings.HasPrefix(rel, Workspace+"/") {
		p = path.Clean(rel)
	}
	if p != Workspace && !strings.HasPrefix(p, Workspace+"/") {
		return "", fmt.Errorf("path %q is outside %s", rel, Workspace)
	}
	return p, nil
}

type ExecRequest struct {
	Argv    []string
	Stdin   []byte
	Timeout time.Duration
}

type ExecResult struct {
	ExitCode        int
	Stdout, Stderr  []byte
	StdoutTruncated bool
	StderrTruncated bool
	TimedOut        bool
}

// Provider 管理沙箱的计算资源与工作区。实现见 docker 子包（开发）；生产实现为 Kubernetes + gVisor（ADR-0010）。
type Provider interface {
	// Ensure 确保沙箱在运行，并挂载其工作区（不存在时创建）。幂等。
	Ensure(ctx context.Context, id string) error
	// Exec 在工作区中执行命令；超时在沙箱内强制执行。
	Exec(ctx context.Context, id string, req ExecRequest) (*ExecResult, error)
	WriteFile(ctx context.Context, id, path string, data []byte) error
	// ReadFile 至多读取 max 字节；文件更长时 truncated 为 true。
	ReadFile(ctx context.Context, id, path string, max int) (data []byte, truncated bool, err error)
	// Stop 释放计算资源，保留工作区。
	Stop(ctx context.Context, id string) error
	// Destroy 删除沙箱及其工作区。
	Destroy(ctx context.Context, id string) error
}

// Activity 记录沙箱的最近活跃时间，供空闲回收使用；多个控制器实例共享。
type Activity interface {
	Touch(ctx context.Context, id string, at time.Time) error
	// ClaimIdle 认领最近活跃早于 before 的沙箱（删除其记录并返回），至多 limit 个。
	ClaimIdle(ctx context.Context, before time.Time, limit int) ([]string, error)
}

// LimitedBuffer 只保留前 Max 个字节，其余丢弃并标记截断。
type LimitedBuffer struct {
	Max       int
	buf       bytes.Buffer
	Truncated bool
}

func (b *LimitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := b.Max - b.buf.Len(); room < len(p) {
		b.Truncated = true
		if room < 0 {
			room = 0
		}
		p = p[:room]
	}
	b.buf.Write(p)
	return n, nil
}

func (b *LimitedBuffer) Bytes() []byte { return b.buf.Bytes() }
