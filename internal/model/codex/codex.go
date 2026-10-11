// Package codex 经本机的 Codex CLI（codex exec）调用 OpenAI 的模型，用作评测的评分模型（ADR-0018）：评分模型须与
// 被评测模型不同系列，而本机的 Codex 以 ChatGPT 账号登录，没有可直接调用的 API 密钥。它只注册在评测命令中，
// 不用于 serve：每次调用起一个进程，不流式、不计量、不支持工具。
package codex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"yanshi/internal/model"
)

// Provider 的模型 ID 形如 "gpt-6.1-sol:high"：冒号后是推理强度（model_reasoning_effort），可省略。
type Provider struct {
	// Bin 是 codex 可执行文件，默认在 PATH 中查找。
	Bin string
}

var _ model.Provider = Provider{}

func (p Provider) Generate(ctx context.Context, req *model.Request, onDelta func(model.Delta)) (*model.Response, error) {
	if len(req.Tools) > 0 {
		return nil, errors.New("codex provider does not support tools")
	}
	modelID, effort, _ := strings.Cut(req.Model, ":")
	// 在空的临时目录中以只读沙箱运行：评分只需要读提示词，不应看到也不能改动任何文件。不读用户的 Codex 配置，
	// 使评分不受个人设置（人格、服务档位等）影响。
	dir, err := os.MkdirTemp("", "yanshi-codex-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "last-message.txt")
	args := []string{"exec", "--ignore-user-config", "--ignore-rules", "--skip-git-repo-check", "--ephemeral",
		"-s", "read-only", "-C", dir, "-o", out}
	if modelID != "" {
		args = append(args, "-m", modelID)
	}
	if effort != "" {
		args = append(args, "-c", "model_reasoning_effort="+effort)
	}
	args = append(args, "-")
	bin := p.Bin
	if bin == "" {
		bin = "codex"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(prompt(req))
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stderr, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// 输出可能回显提示词（含评测内容），只带最后一行。
		lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
		return nil, fmt.Errorf("codex %s: %w: %s", req.Model, err, lines[len(lines)-1])
	}
	b, err := os.ReadFile(out)
	if err != nil {
		return nil, fmt.Errorf("codex %s: no reply: %w", req.Model, err)
	}
	text := strings.TrimSpace(string(b))
	if onDelta != nil {
		onDelta(model.Delta{Text: text})
	}
	return &model.Response{Content: model.TextBlocks(text), Model: req.Model}, nil
}

// prompt 把系统指令与对话拼成一段提示词：Codex CLI 只接受一段初始指令。
func prompt(req *model.Request) string {
	var b strings.Builder
	if req.System != "" {
		b.WriteString(req.System)
		b.WriteString("\n\n")
	}
	for _, m := range req.Messages {
		if len(req.Messages) > 1 {
			fmt.Fprintf(&b, "[%s]\n", m.Role)
		}
		b.WriteString(model.Text(m.Content))
		b.WriteString("\n\n")
	}
	return strings.TrimSpace(b.String())
}
