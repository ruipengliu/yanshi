package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"yanshi/internal/agentdef"
	"yanshi/internal/auth"
	"yanshi/internal/extloop"
)

func TestCheckLoopAgents(t *testing.T) {
	agents, err := agentdef.NewRegistry(
		&agentdef.Def{Name: "planner", Version: "1", Loop: &agentdef.LoopRef{Server: "main"}, Context: agentdef.Context{Scope: agentdef.ScopeRun}},
		&agentdef.Def{Name: "chat", Version: "1", Model: "m"},
		&agentdef.Def{Name: "governed", Version: "1", Loop: &agentdef.LoopRef{Server: "gov", Models: []string{"p/m"}}, Context: agentdef.Context{Scope: agentdef.ScopeRun}},
	)
	if err != nil {
		t.Fatal(err)
	}
	line := func(name string, allow ...string) auth.BusinessLine {
		return auth.BusinessLine{Name: name, Agents: auth.AgentSettings{Allow: allow}}
	}
	mainFor := func(bl string) extloop.Server { return extloop.Server{Name: "main", BusinessLine: bl} }
	keys := []extloop.TokenKey{{KID: "k1", Secret: strings.Repeat("s", 32)}}
	gov := extloop.Server{Name: "gov", BusinessLine: "a", TokenKeys: keys}
	for _, tc := range []struct {
		name     string
		lines    []auth.BusinessLine
		loops    []extloop.Server
		wantErr  string
		wantWarn bool
	}{
		{name: "registered for the line that names it", lines: []auth.BusinessLine{line("a", "planner", "chat")}, loops: []extloop.Server{gov, mainFor("a")}},
		{name: "not registered anywhere", lines: nil, loops: []extloop.Server{gov}, wantErr: `loop "main" is not registered for any business line`},
		{name: "auth none with a registration", lines: nil, loops: []extloop.Server{gov, mainFor("a")}},
		{name: "named by a line without the loop", lines: []auth.BusinessLine{line("a", "planner"), line("b", "planner")}, loops: []extloop.Server{gov, mainFor("a")},
			wantErr: `business line "b" allows agent planner@1, but loop "main" is not registered for it`},
		{name: "glob only warns", lines: []auth.BusinessLine{line("a", "planner"), line("dev", "*")}, loops: []extloop.Server{gov, mainFor("a")}, wantWarn: true},
		{name: "loop.models without token keys", lines: nil, loops: []extloop.Server{mainFor("a"), {Name: "gov", BusinessLine: "b"}},
			wantErr: `agent governed@1 declares loop.models, but loop "gov" of business line "b" has no token_keys`},
		{name: "not allowed is fine", lines: []auth.BusinessLine{line("a", "planner"), line("b", "chat")}, loops: []extloop.Server{gov, mainFor("a")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			err := checkLoopAgents(tc.lines, agents, tc.loops, slog.New(slog.NewTextHandler(&logs, nil)))
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("want error %q, got %v", tc.wantErr, err)
			}
			if warned := strings.Contains(logs.String(), "level=WARN"); warned != tc.wantWarn {
				t.Fatalf("warned = %v, want %v; logs:\n%s", warned, tc.wantWarn, logs.String())
			}
		})
	}
}
