package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yanshi/internal/agentdef"
	"yanshi/internal/auth"
	"yanshi/internal/extloop"
	"yanshi/internal/oneshot"
	"yanshi/internal/usage"
)

func TestServeRejectsInsufficientModelConcurrencyWithOneshots(t *testing.T) {
	dir := t.TempDir()
	declaration := "name: brief\nversion: \"1\"\nmodel: fake/model\ninstructions: 概括\ninput: {max_chars: 100}\nmax_output_tokens: 20\nconcurrency: 1\n"
	if err := os.WriteFile(filepath.Join(dir, "brief.yaml"), []byte(declaration), 0o600); err != nil {
		t.Fatal(err)
	}
	err := serve([]string{"-agents=" + t.TempDir(), "-oneshots=" + dir, "-model-concurrency=1", "-model-min-background=0", "-storage=invalid"})
	if err == nil || !strings.HasPrefix(err.Error(), "oneshot concurrency") {
		t.Fatalf("expected oneshot capacity validation after loading declarations: %v", err)
	}
}

func TestServeSkipsOneshotValidationWithoutDeclarations(t *testing.T) {
	for _, name := range []string{"missing", "empty"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if name == "missing" {
				dir = filepath.Join(dir, "missing")
			}
			// 用无效存储作为检查终点，避免参数通过后启动服务或访问外部依赖。
			err := serve([]string{"-agents=" + t.TempDir(), "-oneshots=" + dir, "-model-concurrency=1", "-model-min-background=0", "-oneshot-concurrency=0", "-businesslines=" + t.TempDir(), "-storage=invalid"})
			if err == nil || !strings.HasPrefix(err.Error(), "unknown storage") {
				t.Fatalf("expected unused oneshot limits to be ignored: %v", err)
			}
		})
	}
}

func TestMeteringRequiresOneshotPrices(t *testing.T) {
	agents, err := agentdef.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	shots, err := oneshot.NewRegistry(&oneshot.Def{Name: "brief", Version: "1", Model: "fake/model:minimal", Instructions: "概括", Input: oneshot.Input{MaxChars: 100}, Concurrency: 1, MaxOutputTokens: 20})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pricing.yaml")
	lines := []auth.BusinessLine{{Name: "bl", Quota: usage.Limits{EndUserDaily: 1}}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := &storage{}
	if _, _, err := metering(path, lines, agents, nil, b, logger, shots); err == nil {
		t.Fatal("unpriced oneshot bypassed quota")
	}
	if err := os.WriteFile(path, []byte("models:\n  fake/model: {input: 1, output: 2}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := metering(path, lines, agents, nil, b, logger, shots); err != nil {
		t.Fatal(err)
	}
}

// 外部循环按步计价（ADR-0062 Consequences）：配置了配额时，登记的每个循环都须有价格，否则它的步数绕过金额配额。
func TestMeteringRequiresLoopPrices(t *testing.T) {
	agents, err := agentdef.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	loops := []extloop.Server{{Name: "plan", BusinessLine: "bl", URL: "https://loop.example"}}
	path := filepath.Join(t.TempDir(), "pricing.yaml")
	lines := []auth.BusinessLine{{Name: "bl", Quota: usage.Limits{EndUserDaily: 1}}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := &storage{}
	if err := os.WriteFile(path, []byte("loops:\n  other/plan: {per_step: 0.01}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := metering(path, lines, agents, loops, b, logger); err == nil || !strings.Contains(err.Error(), "loop bl/plan") {
		t.Fatalf("unpriced loop bypassed quota: %v", err)
	}
	if err := os.WriteFile(path, []byte("loops:\n  bl/plan: {per_step: 0.01}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := metering(path, lines, agents, loops, b, logger); err != nil {
		t.Fatal(err)
	}
}
