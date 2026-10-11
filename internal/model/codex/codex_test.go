package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yanshi/internal/model"
)

// 用一个假的 codex 可执行文件检查参数与回复的读取；真实调用需要登录的 Codex CLI，不在测试中进行。
func TestGenerateRunsCodexExec(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	script := `#!/bin/sh
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"
done
echo "$@" > "` + filepath.Join(dir, "args") + `"
cat > "` + filepath.Join(dir, "stdin") + `"
printf '{"score": 5, "reason": "ok"}' > "$out"
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	resp, err := Provider{Bin: bin}.Generate(context.Background(), &model.Request{Model: "gpt-6.1-sol:high", System: "你是评测员",
		Messages: []model.Message{{Role: model.RoleUser, Content: model.TextBlocks("评分标准：…")}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := model.Text(resp.Content); got != `{"score": 5, "reason": "ok"}` {
		t.Fatalf("reply %q", got)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	for _, want := range []string{"exec", "--ignore-user-config", "-s read-only", "-m gpt-6.1-sol", "model_reasoning_effort=high"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("args %q missing %q", args, want)
		}
	}
	stdin, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	if !strings.HasPrefix(string(stdin), "你是评测员") || !strings.Contains(string(stdin), "评分标准") {
		t.Fatalf("prompt %q", stdin)
	}
}
