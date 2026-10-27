package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNestedModules(t *testing.T) {
	t.Chdir(t.TempDir())
	writeFixture(t, "go.mod", "module yanshi\n\ngo 1.27.1\n")
	writeFixture(t, "persona/go.mod", "module persona\n\ngo 1.27.1\n")
	writeFixture(t, "persona/knowledge/entity.go", "package knowledge\ntype Entity struct { Name string }\n")
	writeFixture(t, "apps/xinli/go.mod", "module xinli\n\ngo 1.27.1\n")
	writeFixture(t, "apps/xinli/doc.go", "package xinli\n")
	ix, err := buildIndex(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.pkgs) != 2 || ix.pkgs["knowledge"] == nil || ix.pkgs["xinli"] == nil {
		t.Fatalf("应按源码包名登记嵌套模块: %v", ix.pkgs)
	}
	for _, ref := range []string{"knowledge.Entity", "persona/knowledge.Entity.Name", "persona/knowledge/", "apps/xinli/doc.go:1"} {
		if msg := ix.checkSpan(ref); msg != "" {
			t.Errorf("有效引用 %q: %s", ref, msg)
		}
	}
	for _, ref := range []string{"knowledge.Missing", "persona/knowledge.Entity.Missing", "persona/missing.go", "apps/xinli/missing.go"} {
		if msg := ix.checkSpan(ref); msg == "" {
			t.Errorf("未报告失效引用 %q", ref)
		}
	}
}

func TestLayerGlossaries(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, path := range []string{"CONTEXT.md", "persona/CONTEXT.md", "apps/xinli/CONTEXT.md", "docs/design/persona-architecture.md", "docs/review/history.md"} {
		writeFixture(t, path, "# 术语\n")
	}
	files, err := docFiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"persona/CONTEXT.md", "apps/xinli/CONTEXT.md"} {
		if !slices.Contains(files, path) {
			t.Errorf("未扫描术语表 %s", path)
		}
	}
	if slices.Contains(files, "docs/review/history.md") {
		t.Error("历史评审不应纳入检查")
	}
	for _, link := range []struct{ file, target string }{
		{"persona/CONTEXT.md", "../CONTEXT.md"},
		{"persona/CONTEXT.md", "../docs/design/persona-architecture.md"},
		{"apps/xinli/CONTEXT.md", "../../CONTEXT.md"},
		{"apps/xinli/CONTEXT.md", "../../persona/CONTEXT.md"},
	} {
		if msg := checkLink(link.file, link.target); msg != "" {
			t.Errorf("%s: %s", link.file, msg)
		}
	}
}
