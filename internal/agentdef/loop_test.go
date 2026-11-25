package agentdef

import "testing"

// loop 与 model 互斥；按 Session 组装上下文的外部循环须配摘要模型（ADR-0062 第 6 条）。
func TestLoopValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		def Def
		ok  bool
	}{
		"model":                   {Def{Name: "a", Version: "1", Model: "p/m"}, true},
		"neither":                 {Def{Name: "a", Version: "1"}, false},
		"both":                    {Def{Name: "a", Version: "1", Model: "p/m", Loop: &LoopRef{Server: "plan"}}, false},
		"loop without server":     {Def{Name: "a", Version: "1", Loop: &LoopRef{}, Context: Context{Scope: ScopeRun}}, false},
		"loop without summary":    {Def{Name: "a", Version: "1", Loop: &LoopRef{Server: "plan"}}, false},
		"loop with summary model": {Def{Name: "a", Version: "1", Loop: &LoopRef{Server: "plan"}, Context: Context{SummaryModel: "p/m"}}, true},
		"loop scoped to run":      {Def{Name: "a", Version: "1", Loop: &LoopRef{Server: "plan"}, Context: Context{Scope: ScopeRun}}, true},
	} {
		t.Run(name, func(t *testing.T) {
			d := tc.def
			if err := d.validate(); (err == nil) != tc.ok {
				t.Fatalf("validate: %v", err)
			}
		})
	}
}
