package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/artifact"
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
		{
			Name: "import_file", Risk: v1.Risk_RISK_LOW, Idempotent: true, TimeoutSeconds: timeout,
			Description:     "把一个 artifact（如用户上传或设备提供的文件）复制到沙箱工作区，供代码处理。",
			InputSchemaJson: `{"type":"object","properties":{"artifact":{"type":"string","description":"artifact ID，如 art_xxx"},"path":{"type":"string","description":"工作区内的目标路径"}},"required":["artifact","path"]}`,
		},
		{
			Name: "export_file", Risk: v1.Risk_RISK_LOW, Idempotent: true, TimeoutSeconds: timeout,
			Description:     "把沙箱工作区中的文件（如生成的图表、表格）导出为 artifact，以便交给用户或传到设备。",
			InputSchemaJson: `{"type":"object","properties":{"path":{"type":"string"},"name":{"type":"string","description":"可选的文件名"}},"required":["path"]}`,
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
	// 进程输出是任意字节，且按字节截断，可能不是合法的 UTF-8。
	return strings.ToValidUTF8(b.String(), "\uFFFD")
}

// SessionOf 返回沙箱所属的 Session。
func SessionOf(sandboxID string) string { return strings.TrimPrefix(sandboxID, NodePrefix) }

// Capabilities 把沙箱能力绑定到 Provider 与工件存储，供控制器的 Executor 使用。
func Capabilities(p Provider, arts *artifact.Service) []nodesdk.Capability {
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
			if err := p.CopyIn(ctx, sandboxID(ctx), path, bytes.NewReader([]byte(in.Content))); err != nil {
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
			out := strings.ToValidUTF8(string(data), "\uFFFD")
			if truncated {
				out += fmt.Sprintf("\n[truncated to %d bytes]", ReadLimit)
			}
			return text(out), nil
		}},
		{Spec: specByName("import_file"), Handler: func(ctx context.Context, args string) ([]*v1.ContentBlock, error) {
			var in struct{ Artifact, Path string }
			if err := json.Unmarshal([]byte(args), &in); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			dst, err := WorkspacePath(in.Path)
			if err != nil {
				return nil, err
			}
			id, _ := artifact.ParseURI(in.Artifact)
			if id == "" {
				id = in.Artifact
			}
			m, rc, err := arts.Open(ctx, id)
			if err != nil {
				return nil, fmt.Errorf("artifact %s: %w", id, err)
			}
			defer rc.Close()
			// 工件只能导入其所属 Session 的沙箱。
			if m.SessionID != SessionOf(sandboxID(ctx)) {
				return nil, fmt.Errorf("artifact %s: %w", id, artifact.ErrNotFound)
			}
			if err := p.CopyIn(ctx, sandboxID(ctx), dst, rc); err != nil {
				return nil, err
			}
			return text(fmt.Sprintf("imported %s (%s) to %s", m.Name, artifact.HumanSize(m.Size), dst)), nil
		}},
		{Spec: specByName("export_file"), Handler: func(ctx context.Context, args string) ([]*v1.ContentBlock, error) {
			var in struct{ Path, Name string }
			if err := json.Unmarshal([]byte(args), &in); err != nil {
				return nil, fmt.Errorf("invalid arguments: %w", err)
			}
			src, err := WorkspacePath(in.Path)
			if err != nil {
				return nil, err
			}
			name := in.Name
			if name == "" {
				name = path.Base(src)
			}
			pr, pw := io.Pipe()
			go func() { pw.CloseWithError(p.CopyOut(ctx, sandboxID(ctx), src, pw)) }()
			m, err := arts.Put(ctx, SessionOf(sandboxID(ctx)), name, "", pr)
			pr.CloseWithError(err)
			if err != nil {
				return nil, err
			}
			block := m.Block()
			return []*v1.ContentBlock{text("exported " + artifact.Describe(block.GetMedia()))[0], block}, nil
		}},
	}
}
