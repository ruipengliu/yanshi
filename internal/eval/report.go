package eval

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"os"
	"slices"
	"sort"
	"strings"
	"time"
)

type Report struct {
	StartedAt     time.Time    `json:"started_at"`
	Judge         string       `json:"judge"`
	ModelOverride string       `json:"model_override,omitempty"`
	Cases         []CaseResult `json:"cases"`
	// LeakedSandboxes 是结束时销毁失败的沙箱 ID，需要手动清理；不写入报告文件与基线。
	LeakedSandboxes []string `json:"-"`
}

// CaseResult 是一个用例在多次运行上的汇总。
type CaseResult struct {
	Name          string  `json:"name"`
	XinliSpanRuns bool    `json:"xinli_span_runs,omitempty"`
	Skipped       string  `json:"skipped,omitempty"`
	Trials        int     `json:"trials"`
	Passed        int     `json:"passed"`
	PassRate      float64 `json:"pass_rate"`
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
	// InfraErrors 是因评测设施失败（如评分模型不可用）而无法判定的运行次数；这样的结果不写入基线。
	InfraErrors int `json:"infra_errors,omitempty"`
	// Samples 是失败的示例（每种失败一条），便于定位。
	Samples []string `json:"samples,omitempty"`
	// Metrics 是各轮指标在多次运行上的均值与标准差（键为"turn N: 名称"，m4-eval.md §13）。
	Metrics map[string]Metric `json:"metrics,omitempty"`

	values                        map[string][]float64
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
	for k, v := range t.Metrics {
		if r.values == nil {
			r.values = map[string][]float64{}
		}
		r.values[k] = append(r.values[k], v)
	}
	if t.Err != "" {
		r.fail("error", t.Err)
	}
	infra := false
	for _, a := range t.Assertions {
		if !a.Pass {
			r.fail(fmt.Sprintf("turn %d: %s", a.Turn, a.Name), a.Detail)
			infra = infra || a.Infra
		}
	}
	if infra {
		r.InfraErrors++
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
	for k, vs := range r.values {
		if r.Metrics == nil {
			r.Metrics = map[string]Metric{}
		}
		r.Metrics[k] = newMetric(vs)
	}
	sort.Strings(r.Samples)
}

// Metric 是一项指标在多次运行上的均值、样本标准差与运行次数（没有算出这项指标的运行不计）。
type Metric struct {
	Mean float64 `json:"mean"`
	Std  float64 `json:"std"`
	N    int     `json:"n"`
}

func newMetric(vs []float64) Metric {
	m := Metric{N: len(vs)}
	for _, v := range vs {
		m.Mean += v
	}
	m.Mean /= float64(m.N)
	if m.N > 1 {
		for _, v := range vs {
			m.Std += (v - m.Mean) * (v - m.Mean)
		}
		m.Std = math.Sqrt(m.Std / float64(m.N-1))
	}
	return m
}

func (m Metric) String() string { return fmt.Sprintf("%.2f ± %.2f (%d)", m.Mean, m.Std, m.N) }

// lowerIsBetter 是越小越好的指标（按"turn N: "之后的名称）。
var lowerIsBetter = map[string]bool{"fragmentation": true}

// worse 报告 cur 是否比基线 base 明显变差：变差的幅度至少为 MetricDrop，且超过两倍的标准误（多次运行的波动）。
func worse(name string, base, cur Metric) bool {
	drop := base.Mean - cur.Mean
	if _, short, _ := strings.Cut(name, ": "); lowerIsBetter[short] {
		drop = -drop
	}
	se := math.Sqrt(base.Std*base.Std/float64(base.N) + cur.Std*cur.Std/float64(cur.N))
	return drop >= max(MetricDrop, 2*se)
}

// 回归阈值（ADR-0018）：偏宽，以减少由于输出波动造成的误报。
const (
	PassRateDrop = 0.34
	JudgeDrop    = 0.5
	MetricDrop   = 0.1
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
		for _, k := range slices.Sorted(maps.Keys(c.Metrics)) {
			if bm, ok := b.Metrics[k]; ok && bm.N > 0 && worse(k, bm, c.Metrics[k]) {
				out = append(out, fmt.Sprintf("%s: %s %s → %s", c.Name, k, bm, c.Metrics[k]))
			}
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
	var spans []string
	for _, c := range r.Cases {
		if c.XinliSpanRuns {
			spans = append(spans, c.Name)
		}
	}
	if len(spans) > 0 {
		fmt.Fprintf(&b, "实验：以下用例的 xinli-day 每个完整时段一个 Run，当天整理另开 Run：%s。\n\n", strings.Join(spans, "、"))
	}
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
	var measured []CaseResult
	for _, c := range r.Cases {
		if len(c.Metrics) > 0 {
			measured = append(measured, c)
		}
	}
	if len(measured) > 0 {
		b.WriteString("\n## 指标\n\n均值 ± 标准差（运行次数）。\n\n| 用例 | 指标 | 本次 | 基线 |\n|---|---|---|---|\n")
		for _, c := range measured {
			for _, k := range slices.Sorted(maps.Keys(c.Metrics)) {
				bm := ""
				if m, ok := base[c.Name].Metrics[k]; ok {
					bm = m.String()
				}
				fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", c.Name, k, c.Metrics[k], bm)
			}
		}
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

// MergeBaseline 用 current 中的用例替换或补充 baseline，其余用例保留；baseline 为 nil 时视为空。
// 有评测设施失败的用例（InfraErrors > 0）不写入，保留原基线，并在 skipped 中返回其名称：
// 评分模型不可用不代表 Agent 变差，不能被记成新的基线。
func MergeBaseline(baseline, current *Report) (merged *Report, skipped []string) {
	out := *current
	out.Cases = nil
	seen := map[string]bool{}
	for _, c := range current.Cases {
		if c.InfraErrors > 0 {
			skipped = append(skipped, c.Name)
			continue
		}
		seen[c.Name] = true
		out.Cases = append(out.Cases, c)
	}
	if baseline != nil {
		for _, c := range baseline.Cases {
			if !seen[c.Name] {
				out.Cases = append(out.Cases, c)
			}
		}
	}
	sort.Slice(out.Cases, func(i, j int) bool { return out.Cases[i].Name < out.Cases[j].Name })
	return &out, skipped
}
