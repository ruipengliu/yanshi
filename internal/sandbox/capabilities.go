package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/sdk/nodesdk"
)

const (
	// OutputLimit 是返回给模型的 stdout / stderr 各自的上限。
	OutputLimit = 16 << 10
	// ReadLimit 是 read_file 的上限。
	ReadLimit = 64 << 10
	// DefaultExecTimeout 与 MaxExecTimeout 约束单次执行时长（S1：代码解释器定位）。
	DefaultExecTimeout = 60 * time.Second
	MaxExecTimeout     = 5 * time.Minute
	// CallTimeout 是路由调用的结果截止时长，含排队与沙箱创建。
	CallTimeout = 15 * time.Minute
)

type ctxKey struct{}
type callKey struct{}

// WithSandbox 把当前沙箱 ID 与调用 ID 放入 context，供能力处理函数与 Provider 使用。
func WithSandbox(ctx context.Context, id, callID string) context.Context {
	return context.WithValue(context.WithValue(ctx, ctxKey{}, id), callKey{}, callID)
}

func sandboxID(ctx context.Context) string { s, _ := ctx.Value(ctxKey{}).(string); return s }

// CallID 返回当前执行的调用 ID（可作为幂等键或日志字段）。
func CallID(ctx context.Context) string { s, _ := ctx.Value(callKey{}).(string); return s }

func text(s string) []*v1.ContentBlock {
	return []*v1.ContentBlock{{Kind: &v1.ContentBlock_Text{Text: &v1.Text{Text: s}}}}
}

// Specs 是沙箱对模型暴露的能力声明。
func Specs() []*v1.CapabilitySpec {
	timeout := uint32(CallTimeout / time.Second)
	execProps := `"timeout_seconds":{"type":"integer","description":"默认 60，最大 300"}`
	return []*v1.CapabilitySpec{
		{
			Name: "run_python", Risk: v1.Risk_RISK_LOW, TimeoutSeconds: timeout,
			Description:     "在本会话的云端沙箱中执行 Python 3 代码（已预装 pandas、numpy、matplotlib）。工作目录 /workspace 跨调用保留；沙箱无网络。返回退出码与 stdout/stderr。",
			InputSchemaJson: `{"type":"object","properties":{"code":{"type":"string"},` + execProps + `},"required":["code"]}`,
		},
		{
			Name: "exec", Risk: v1.Risk_RISK_LOW, TimeoutSeconds: timeout,
			Description:     "在本会话的云端沙箱中执行 shell 命令，工作目录 /workspace。沙箱无网络。",
			InputSchemaJson: `{"type":"object","properties":{"command":{"type":"string"},` + execProps + `},"required":["command"]}`,
		},
		{
			Name: "write_file", Risk: v1.Risk_RISK_LOW, Idempotent: true, TimeoutSeconds: timeout,
			Description:     "在沙箱工作区写入文本文件（覆盖）。路径相对于 /workspace。",
			InputSchemaJson: `{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`,
		},
		{
			Name: "read_file", Risk: v1.Risk_RISK_LOW, Idempotent: true, TimeoutSeconds: timeout,
			Description:     "读取沙箱工作区中的文本文件（最多 64KB）。路径相对于 /workspace。",
			InputSchemaJson: `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`,
		},
	}
}

func specByName(name string) *v1.CapabilitySpec {
	for _, s := range Specs() {
		if s.GetName() == name {
			return s
		}
	}
	panic("unknown sandbox capability " + name)
}

func execTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		return DefaultExecTimeout
	}
	return min(time.Duration(seconds)*time.Second, MaxExecTimeout)
}

// FormatExec 把执行结果整理为给模型看的文本。
func FormatExec(r *ExecResult) string {
	var b strings.Builder
	if r.TimedOut {
		b.WriteString("timed out (killed)\n")
	}
	fmt.Fprintf(&b, "exit_code: %d\n", r.ExitCode)
	section := func(name string, data []byte, truncated bool) {
		if len(data) == 0 {
			return
		}
		fmt.Fprintf(&b, "--- %s ---\n%s", name, data)
		if !strings.HasSuffix(string(data), "\n") {
			b.WriteString("\n")
		}
		if truncated {
			fmt.Fprintf(&b, "[%s truncated to %d bytes]\n", name, OutputLimit)
		}
	}
	section("stdout", r.Stdout, r.StdoutTruncated)
	section("stderr", r.Stderr, r.StderrTruncated)
	return b.String()
}

// Capabilities 把沙箱能力绑定到 Provider，供控制器的 Executor 使用。
func Capabilities(p Provider) []nodesdk.Capability {
	run := func(ctx context.Context, req ExecRequest) ([]*v1.ContentBlock, error) {
		r, err := p.Exec(ctx, sandboxID(ctx), req)
		if err != nil {
			return nil, err
		}
		return text(FormatExec(r)), nil
	}
	return []nodesdk.Capability{
		{Spec: specByName("run_python"), Handler: func(ctx context.Context, args string) ([]*v1.ContentBlock, error) {
			var in struct {
				Code    string `json:"code"`
				Timeout int    `json:"timeout_seconds"`
			}
			if err := json.Unmarshal([]byte(args), &in); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			return run(ctx, ExecRequest{Argv: []string{"python3", "-"}, Stdin: []byte(in.Code), Timeout: execTimeout(in.Timeout)})
		}},
		{Spec: specByName("exec"), Handler: func(ctx context.Context, args string) ([]*v1.ContentBlock, error) {
			var in struct {
				Command string `json:"command"`
				Timeout int    `json:"timeout_seconds"`
			}
			if err := json.Unmarshal([]byte(args), &in); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			return run(ctx, ExecRequest{Argv: []string{"sh", "-c", in.Command}, Timeout: execTimeout(in.Timeout)})
		}},
		{Spec: specByName("write_file"), Handler: func(ctx context.Context, args string) ([]*v1.ContentBlock, error) {
			var in struct{ Path, Content string }
			if err := json.Unmarshal([]byte(args), &in); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			path, err := WorkspacePath(in.Path)
			if err != nil {
				return nil, err
			}
			if err := p.WriteFile(ctx, sandboxID(ctx), path, []byte(in.Content)); err != nil {
				return nil, err
			}
			return text(fmt.Sprintf("wrote %d bytes to %s", len(in.Content), path)), nil
		}},
		{Spec: specByName("read_file"), Handler: func(ctx context.Context, args string) ([]*v1.ContentBlock, error) {
			var in struct{ Path string }
			if err := json.Unmarshal([]byte(args), &in); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			path, err := WorkspacePath(in.Path)
			if err != nil {
				return nil, err
			}
			data, truncated, err := p.ReadFile(ctx, sandboxID(ctx), path, ReadLimit)
			if err != nil {
				return nil, err
			}
			out := string(data)
			if truncated {
				out += fmt.Sprintf("\n[truncated to %d bytes]", ReadLimit)
			}
			return text(out), nil
		}},
	}
}
