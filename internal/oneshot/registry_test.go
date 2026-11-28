package oneshot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yanshi/internal/agentdef"
)

func definition() *Def {
	return &Def{Name: "brief", Version: "1", Model: "fake/model", Instructions: "概括输入。", Input: Input{MaxChars: 100}, MaxOutputTokens: 50, Concurrency: 2}
}

func schemaDef() *Def {
	d := definition()
	d.Output = map[string]any{"type": "object", "properties": map[string]any{"brief": map[string]any{"type": "string"}}, "required": []string{"brief"}, "additionalProperties": false}
	return d
}

func TestDeclarationValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Def)
	}{
		{"name", func(d *Def) { d.Name = "Bad/name" }},
		{"version", func(d *Def) { d.Version = "" }},
		{"model", func(d *Def) { d.Model = "bad" }},
		{"instructions", func(d *Def) { d.Instructions = "" }},
		{"input", func(d *Def) { d.Input.MaxChars = 0 }},
		{"tokens", func(d *Def) { d.MaxOutputTokens = 0 }},
		{"concurrency", func(d *Def) { d.Concurrency = 0 }},
		{"work_class", func(d *Def) { d.WorkClass = "urgent" }},
		{"kind", func(d *Def) { d.Kind = "embedding" }},
		{"schema", func(d *Def) { d.Output = map[string]any{"type": "not-a-type"} }},
		{"ref", func(d *Def) { d.Output = map[string]any{"$ref": "https://example.invalid/schema"} }},
		{"nested_ref", func(d *Def) {
			d.Output = map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"$ref": "local.json"}}}
		}},
		{"dynamic_ref", func(d *Def) { d.Output = map[string]any{"$dynamicRef": "file:///tmp/schema"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := definition()
			tc.change(d)
			if _, err := NewRegistry(d); err == nil {
				t.Fatal("accepted invalid declaration")
			}
		})
	}
	a, b := definition(), definition()
	b.Version, b.Concurrency = "2", 3
	if _, err := NewRegistry(a, b); err == nil {
		t.Fatal("accepted different concurrency")
	}
	if _, err := NewRegistry(a, a); err == nil {
		t.Fatal("accepted duplicate version")
	}
}

func TestSchemaLocalReferencesAndBoolean(t *testing.T) {
	for _, tc := range []struct {
		schema    any
		good, bad string
	}{
		{schemaDef().Output, `{"brief":"好"}`, `{"brief":3}`},
		{map[string]any{"$defs": map[string]any{"text": map[string]any{"type": "string"}}, "$ref": "#/$defs/text"}, `"text"`, `123`},
		{true, `null`, `not json`},
		{false, "", `null`},
	} {
		d := definition()
		d.Output = tc.schema
		if _, err := NewRegistry(d); err != nil {
			t.Fatal(err)
		}
		if tc.good != "" {
			if _, category := d.ValidateOutput(tc.good); category != "" {
				t.Fatalf("valid output: %s", category)
			}
		}
		if _, category := d.ValidateOutput(tc.bad); category == "" {
			t.Fatal("invalid output accepted")
		}
	}
	d := schemaDef()
	if _, err := NewRegistry(d); err != nil {
		t.Fatal(err)
	}
	if _, category := d.ValidateOutput(`{"brief":"secret","extra":"secret"}`); category != "schema_mismatch" {
		t.Fatalf("category: %s", category)
	}
}

func TestSchemaTypeValidation(t *testing.T) {
	for _, typ := range []string{"null", "boolean", "object", "array", "number", "string", "integer"} {
		t.Run(typ, func(t *testing.T) {
			d := definition()
			d.Output = map[string]any{"type": typ}
			if _, err := NewRegistry(d); err != nil {
				t.Fatalf("rejected valid schema type: %v", err)
			}
		})
	}
	for _, types := range [][]string{{"string", "null"}, {"string", "impossible"}, {""}, {}} {
		t.Run(fmt.Sprint(types), func(t *testing.T) {
			d := definition()
			d.Output = map[string]any{"type": "object", "properties": map[string]any{"brief": map[string]any{"type": types}}}
			_, err := NewRegistry(d)
			valid := len(types) == 2 && types[1] == "null"
			if (err == nil) != valid {
				t.Fatalf("schema types %v: %v", types, err)
			}
		})
	}
	d := definition()
	d.Output = map[string]any{"type": "object", "properties": map[string]any{"brief": map[string]any{"type": "impossible"}}}
	if _, err := NewRegistry(d); err == nil {
		t.Fatal("accepted invalid type in a child schema")
	}
}

func TestLoadDirAndSharedReleases(t *testing.T) {
	dir := t.TempDir()
	base := "name: brief\nversion: %q\nmodel: fake/model\ninstructions: 概括输入\ninput: {max_chars: 10}\nmax_output_tokens: 20\nconcurrency: 2\n"
	for _, v := range []string{"1", "2", "3"} {
		if err := os.WriteFile(filepath.Join(dir, "brief-"+v+".yaml"), []byte(fmt.Sprintf(base, v)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	releases := []byte("brief: {stable: \"1\", canary: {version: \"2\", percent: 25, end_users: [qa]}, withdrawn: [\"3\"]}\n")
	path := filepath.Join(dir, ReleasesFile)
	if err := os.WriteFile(path, releases, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	rel, _, err := r.LoadReleases(path)
	if err != nil {
		t.Fatal(err)
	}
	r.SetReleases(rel)
	a, err := agentdef.NewRegistry(&agentdef.Def{Name: "brief", Version: "1", Model: "fake/model"}, &agentdef.Def{Name: "brief", Version: "2", Model: "fake/model"}, &agentdef.Def{Name: "brief", Version: "3", Model: "fake/model"})
	if err != nil {
		t.Fatal(err)
	}
	ar, err := a.ParseReleases(releases)
	if err != nil {
		t.Fatal(err)
	}
	a.SetReleases(ar)
	for i := range 1000 {
		u := fmt.Sprint(i)
		x, err := r.Resolve("brief", u)
		if err != nil {
			t.Fatal(err)
		}
		y, err := a.Resolve("brief", u)
		if err != nil {
			t.Fatal(err)
		}
		if x.Version != y.Version || x.Version == "3" {
			t.Fatal("declarations disagree on rollout")
		}
	}
	d, _ := r.Resolve("brief", "qa")
	if d.Version != "2" {
		t.Fatal("canary allowlist")
	}
	d, _ = r.Default("brief")
	if d.Version != "1" {
		t.Fatal("stable default")
	}
	for _, b := range []string{"brief: {stable: \"4\"}", "brief: {stable: \"1\", withdrawn: [\"1\"]}", "brief: {stable: \"1\", canary: {version: \"3\", percent: 101}}", "brief: {stable: \"1\", canary: {version: \"3\"}, withdrawn: [\"3\"]}"} {
		if _, err := r.ParseReleases([]byte(b)); err == nil {
			t.Fatal("accepted invalid release")
		}
	}
	if _, err := r.Resolve("missing", "u"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte(fmt.Sprintf(base, "4")+"kind: embedding\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDir(dir); err == nil || !strings.Contains(err.Error(), "generate") {
		t.Fatal("kind accepted", err)
	}
	// 空目录和没有发布文件的部署仍可加载。
	empty, err := LoadDir(t.TempDir())
	if err != nil || len(empty.All()) != 0 {
		t.Fatal(err)
	}
	if _, _, err := empty.LoadReleases(filepath.Join(t.TempDir(), ReleasesFile)); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDirs(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	base := "name: %s\nversion: %q\nmodel: fake/model\ninstructions: 概括\ninput: {max_chars: 10}\nmax_output_tokens: 20\nconcurrency: 2\n"
	write := func(dir, file, contents string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, file), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(first, "first.yaml", fmt.Sprintf(base, "brief", "2"))
	write(first, "last.yaml", fmt.Sprintf(base, "brief", "1"))
	write(first, ReleasesFile, "brief: {stable: '2'}\n")
	write(second, "summary.yaml", fmt.Sprintf(base, "summary", "1"))
	write(second, ReleasesFile, "summary: {stable: '1'}\n")
	for _, dirs := range [][]string{{first}, {first, second}} {
		r, files, err := LoadDirs(dirs)
		if err != nil {
			t.Fatal(err)
		}
		if d, err := r.Default("brief"); err != nil || d.Version != "1" {
			t.Fatalf("latest brief = %v, %v", d, err)
		}
		rel, _, err := files.Load()
		if err != nil {
			t.Fatal(err)
		}
		r.SetReleases(rel)
		if len(r.All()) != len(dirs)+1 || len(rel) != len(dirs) {
			t.Fatalf("incomplete merge: %v", rel)
		}
		if d, err := r.Default("brief"); err != nil || d.Version != "2" {
			t.Fatalf("stable brief = %v, %v", d, err)
		}
	}
	write(second, ReleasesFile, "brief: {stable: '2'}\n")
	_, files, err := LoadDirs([]string{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := files.Load(); err == nil || !strings.Contains(err.Error(), filepath.Join(second, ReleasesFile)) {
		t.Fatalf("expected misplaced release to be rejected: %v", err)
	}
	write(second, "duplicate.yaml", fmt.Sprintf(base, "brief", "3"))
	if _, _, err := LoadDirs([]string{first, second}); err == nil || !strings.Contains(err.Error(), "defined in both") {
		t.Fatalf("expected duplicate name to be rejected: %v", err)
	}
}

func TestSchemaPropertyNamedRefIsData(t *testing.T) {
	d := definition()
	d.Output = map[string]any{"type": "object", "properties": map[string]any{"$ref": map[string]any{"type": "string"}}, "required": []string{"$ref"}}
	if _, err := NewRegistry(d); err != nil {
		t.Fatal(err)
	}
	if _, category := d.ValidateOutput(`{"$ref":"https://example.invalid/data"}`); category != "" {
		t.Fatal(category)
	}
}

func TestLoadRejectsMalformedDeclarationFields(t *testing.T) {
	base := "name: brief\nversion: \"1\"\nmodel: fake/model\ninstructions: 概括\ninput: {max_chars: 10}\nmax_output_tokens: 20\nconcurrency: 2\n"
	for _, extra := range []string{"kind: ''\n", "output: null\n", "unknown: true\n", "output: {type: impossible}\n", "output: {$ref: 'https://example.invalid/schema'}\n"} {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "brief.yaml"), []byte(base+extra), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadDir(dir); err == nil {
			t.Fatal("accepted", extra)
		}
	}
}
