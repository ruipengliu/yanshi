package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigureModelLimits(t *testing.T) {
	for _, tc := range []struct {
		name     string
		max, min int
		set      bool
		want     int
		invalid  bool
	}{
		{name: "unlimited"},
		{name: "ceil", max: 9, want: 2},
		{name: "exact", max: 16, want: 2},
		{name: "small", max: 2, want: 1},
		{name: "one needs explicit zero", max: 1, want: 1, invalid: true},
		{name: "one disabled", max: 1, set: true},
		{name: "disabled", max: 16, set: true},
		{name: "explicit", max: 16, min: 3, set: true, want: 3},
		{name: "negative", max: 8, min: -1, set: true, want: -1, invalid: true},
		{name: "equal", max: 8, min: 8, set: true, want: 8, invalid: true},
		{name: "greater", max: 8, min: 9, set: true, want: 9, invalid: true},
		{name: "ignored when unlimited", min: 3, set: true, want: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := defaultModelLimits
			l.MaxConcurrency, l.MinBackground = tc.max, tc.min
			got, err := configureModelLimits(l, tc.set)
			if (err != nil) != tc.invalid || got.MinBackground != tc.want {
				t.Fatalf("limits %+v, err %v", got, err)
			}
			l.MinBackground = tc.want
			if got != l {
				t.Fatalf("other limits changed: got %+v, want %+v", got, l)
			}
		})
	}
}

func TestConfigureProviderModelLimits(t *testing.T) {
	global := defaultModelLimits
	global.MaxConcurrency, global.MinBackground, global.RPS = 16, 2, 10
	fs := flag.NewFlagSet("model-limits", flag.ContinueOnError)
	var specs multiFlag
	fs.Var(&specs, "model-limit", "")
	if err := fs.Parse([]string{
		"-model-limit", "ark:concurrency=1,rps=3.5",
		"-model-limit=tokenhub:rps=0",
		"-model-limit", "ark:concurrency=17",
		"-model-limit", "local:concurrency=1,min-background=0",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := configureProviderModelLimits(global, false, specs)
	if err != nil {
		t.Fatal(err)
	}
	ark, tokenhub, local := global, global, global
	ark.MaxConcurrency, ark.MinBackground, ark.RPS = 17, 3, 3.5
	tokenhub.RPS = 0
	local.MaxConcurrency, local.MinBackground = 1, 0
	if len(got) != 3 || got["ark"] != ark || got["tokenhub"] != tokenhub || got["local"] != local {
		t.Fatalf("provider limits %+v, want ark=%+v tokenhub=%+v local=%+v", got, ark, tokenhub, local)
	}

	got, err = configureProviderModelLimits(global, true, []string{"ark:concurrency=17", "local:concurrency=1", "local:min-background=0"})
	ark.MinBackground, ark.RPS = 2, 10
	if err != nil || got["ark"] != ark || got["local"] != local {
		t.Fatalf("explicit global minimum: limits %+v, err %v, want ark=%+v local=%+v", got, err, ark, local)
	}
}

func TestServeRejectsInvalidProviderModelLimitsBeforeLoadingAgents(t *testing.T) {
	for _, tc := range []struct {
		spec string
		want string
	}{
		{"other:rps=1", "unknown guarded provider"},
		{"ark:retry=1", "unknown key"},
		{"ark:rps=", "want key=value"},
		{"ark:concurrency=1.5", "concurrency must be"},
		{"ark:rps=NaN", "rps must be"},
		{"ark:concurrency=1", "model min background"},
		{"ark:concurrency=2,min-background=2", "model min background"},
	} {
		t.Run(tc.spec, func(t *testing.T) {
			args := []string{"-model-limit=" + tc.spec, "-agents=" + invalidModelAgentsDir(t)}
			if err := serve(args); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("serve(%v): %v, want %q", args, err, tc.want)
			}
		})
	}
}

func TestServeRejectsInvalidModelMinimumBeforeLoadingAgents(t *testing.T) {
	// 参数错误须在读取配置或启动外部依赖之前返回。
	for _, args := range [][]string{
		{"-model-concurrency=1"},
		{"-model-concurrency=8", "-model-min-background=8"},
		{"-model-min-background=-1"},
	} {
		args = append(args, "-agents="+invalidModelAgentsDir(t))
		if err := serve(args); err == nil || !strings.HasPrefix(err.Error(), "model min background") {
			t.Fatalf("serve(%v): %v", args, err)
		}
	}
}

func TestServeAcceptsExplicitZeroModelMinimum(t *testing.T) {
	err := serve([]string{"-model-concurrency=1", "-model-min-background=0", "-agents=" + invalidModelAgentsDir(t)})
	if err == nil || !strings.HasPrefix(err.Error(), "load agents:") {
		t.Fatalf("expected model validation to pass before loading agents: %v", err)
	}
}

func invalidModelAgentsDir(t *testing.T) string {
	t.Helper()
	// 不存在目录会被 LoadDir 视为空目录；用坏 YAML 确保不会启动外部依赖。
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte("["), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}
