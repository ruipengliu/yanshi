package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

type Report struct {
	StartedAt     time.Time    `json:"started_at"`
	Judge         string       `json:"judge"`
	ModelOverride string       `json:"model_override,omitempty"`
	Cases         []CaseResult `json:"cases"`
}

// CaseResult 是一个用例在多次运行上的汇总。
type CaseResult struct {
	Name     string  `json:"name"`
	Skipped  string  `json:"skipped,omitempty"`
	Trials   int     `json:"trials"`
	Passed   int     `json:"passed"`
	PassRate float64 `json:"pass_rate"`
	// JudgeAvg 是评分均值；没有评分断言时为 0。
	JudgeAvg   float64 `json:"judge_avg"`
	AvgTokens  uint64  `json:"avg_tokens"`
	AvgSeconds float64 `json:"avg_seconds"`
	// 每次运行平均的调用、上下文压缩与接管次数（观察长 Run 的形态）。
	AvgCalls       float64 `json:"avg_calls"`
	AvgCompactions float64 `json:"avg_compactions"`
	AvgTakeovers   float64 `json:"avg_takeovers"`
	// CacheHit 是输入 token 中命中前缀缓存的比例（全部运行合计）。
	CacheHit float64        `json:"cache_hit"`
	Failures map[string]int `json:"failures,omitempty"`
	// Samples 是失败的示例（每种失败一条），便于定位。
	Samples []string `json:"samples,omitempty"`

	scores                        []int
	tokens                        uint64
	seconds                       float64
	runs                          int
	calls, compactions, takeovers int
	input, cached                 uint64
}

func (r *CaseResult) add(t TrialResult) {
	r.runs++
	if t.Passed {
		r.Passed++
	}
	r.tokens += t.Tokens
	r.seconds += t.Duration.Seconds()
	r.calls, r.compactions, r.takeovers = r.calls+t.Calls, r.compactions+t.Compactions, r.takeovers+t.Takeovers
	r.input, r.cached = r.input+t.InputTokens, r.cached+t.CachedInputTokens
	r.scores = append(r.scores, t.JudgeScore...)
	if t.Err != "" {
		r.fail("error", t.Err)
	}
	for _, a := range t.Assertions {
		if !a.Pass {
			r.fail(fmt.Sprintf("turn %d: %s", a.Turn, a.Name), a.Detail)
		}
	}
}

func (r *CaseResult) fail(key, detail string) {
	if r.Failures[key] == 0 {
		r.Samples = append(r.Samples, key+" — "+detail)
	}
	r.Failures[key]++
}

func (r *CaseResult) finish() {
	if r.runs == 0 {
		return
	}
	r.PassRate = float64(r.Passed) / float64(r.runs)
	r.AvgTokens = r.tokens / uint64(r.runs)
	r.AvgSeconds = r.seconds / float64(r.runs)
	n := float64(r.runs)
	r.AvgCalls, r.AvgCompactions, r.AvgTakeovers = float64(r.calls)/n, float64(r.compactions)/n, float64(r.takeovers)/n
	if r.input > 0 {
		r.CacheHit = float64(r.cached) / float64(r.input)
	}
	if len(r.scores) > 0 {
		sum := 0
		for _, s := range r.scores {
			sum += s
		}
		r.JudgeAvg = float64(sum) / float64(len(r.scores))
	}
	sort.Strings(r.Samples)
}

// 回归阈值（ADR-0018）：偏宽，以减少由于输出波动造成的误报。
const (
	PassRateDrop = 0.34
	JudgeDrop    = 0.5
)

// Compare 与基线比较，返回回归（通过率或评分明显下降）。基线中没有的用例不算回归。
func Compare(baseline, current *Report) []string {
	base := map[string]CaseResult{}
	for _, c := range baseline.Cases {
		base[c.Name] = c
	}
	var out []string
	for _, c := range current.Cases {
		b, ok := base[c.Name]
		if !ok || c.Skipped != "" || b.Skipped != "" {
			continue
		}
		if b.PassRate-c.PassRate >= PassRateDrop {
			out = append(out, fmt.Sprintf("%s: pass rate %.2f → %.2f", c.Name, b.PassRate, c.PassRate))
		}
		if b.JudgeAvg > 0 && b.JudgeAvg-c.JudgeAvg >= JudgeDrop {
			out = append(out, fmt.Sprintf("%s: judge %.2f → %.2f", c.Name, b.JudgeAvg, c.JudgeAvg))
		}
	}
	return out
}

func LoadReport(path string) (*Report, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	r := &Report{}
	return r, json.Unmarshal(b, r)
}

func (r *Report) WriteJSON(path string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// Markdown 返回人读的报告；regressions 非空时列在开头。
func (r *Report) Markdown(baseline *Report, regressions []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 评测报告 %s\n\n评分模型：%s", r.StartedAt.Format(time.RFC3339), r.Judge)
	if r.ModelOverride != "" {
		fmt.Fprintf(&b, "；被评测模型：%s", r.ModelOverride)
	}
	b.WriteString("\n\n")
	if len(regressions) > 0 {
		b.WriteString("## 回归\n\n")
		for _, x := range regressions {
			fmt.Fprintf(&b, "- %s\n", x)
		}
		b.WriteString("\n")
	}
	base := map[string]CaseResult{}
	if baseline != nil {
		for _, c := range baseline.Cases {
			base[c.Name] = c
		}
	}
	b.WriteString("| 用例 | 通过 | 基线 | 评分 | 基线评分 | 平均 tokens | 缓存命中 | 平均耗时 | 调用/压缩/接管 |\n|---|---|---|---|---|---|---|---|---|\n")
	for _, c := range r.Cases {
		if c.Skipped != "" {
			fmt.Fprintf(&b, "| %s | 跳过（%s） | | | | | | | |\n", c.Name, c.Skipped)
			continue
		}
		bp, bj := "", ""
		if x, ok := base[c.Name]; ok {
			bp, bj = fmt.Sprintf("%d/%d", x.Passed, x.Trials), score(x.JudgeAvg)
		}
		fmt.Fprintf(&b, "| %s | %d/%d | %s | %s | %s | %d | %.0f%% | %.1fs | %.1f/%.1f/%.1f |\n", c.Name, c.Passed, c.Trials, bp, score(c.JudgeAvg), bj,
			c.AvgTokens, 100*c.CacheHit, c.AvgSeconds, c.AvgCalls, c.AvgCompactions, c.AvgTakeovers)
	}
	var failing []CaseResult
	for _, c := range r.Cases {
		if len(c.Samples) > 0 {
			failing = append(failing, c)
		}
	}
	if len(failing) > 0 {
		b.WriteString("\n## 失败示例\n\n")
		for _, c := range failing {
			fmt.Fprintf(&b, "**%s**\n\n", c.Name)
			for _, s := range c.Samples {
				fmt.Fprintf(&b, "- %s\n", s)
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

func score(v float64) string {
	if v == 0 {
		return "—"
	}
	return fmt.Sprintf("%.2f", v)
}

// MergeBaseline 用 current 中的用例替换或补充 baseline，其余用例保留；baseline 为 nil 时返回 current。
func MergeBaseline(baseline, current *Report) *Report {
	if baseline == nil {
		return current
	}
	out := *current
	seen := map[string]bool{}
	for _, c := range current.Cases {
		seen[c.Name] = true
	}
	for _, c := range baseline.Cases {
		if !seen[c.Name] {
			out.Cases = append(out.Cases, c)
		}
	}
	sort.Slice(out.Cases, func(i, j int) bool { return out.Cases[i].Name < out.Cases[j].Name })
	return &out
}
