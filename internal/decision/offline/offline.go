// Package offline 在固定样本集上离线评估判断器（docs/design/m6-decisions.md §9）：不经被测模型与评分模型，
// 直接把样本交给 decision.Decider，报告 AUC、固定假阳性率下的检出率与时延。阈值只在调参集上确定，
// 留出集只用来报告最终数字（ADR-0030 第 4 条）。
package offline

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"time"

	"yanshi/internal/decision"
	"yanshi/internal/model"
)

// 样本的标签。
const (
	Injection = "injection"
	Clean     = "clean"
)

// Sample 是一条样本：一段调用结果的文字及其标签。同一模板派生的样本（同一段内容的注入版、干净版与难负例）
// 必须落在同一个划分中，否则判断器可能学到的是话题而不是注入。
type Sample struct {
	ID        string `json:"id"`
	Template  string `json:"template"`
	Source    string `json:"source"`
	Label     string `json:"label"`
	Technique string `json:"technique"`
	Text      string `json:"text"`
}

// Load 读取 JSONL 样本文件，校验标签与 ID 唯一。
func Load(paths ...string) ([]Sample, error) {
	var out []Sample
	seen := map[string]bool{}
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for n := 1; sc.Scan(); n++ {
			if len(sc.Bytes()) == 0 {
				continue
			}
			var s Sample
			if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
				f.Close()
				return nil, fmt.Errorf("%s:%d: %w", path, n, err)
			}
			if s.Label != Injection && s.Label != Clean || s.ID == "" || s.Template == "" || s.Text == "" {
				f.Close()
				return nil, fmt.Errorf("%s:%d: sample needs id, template, text and label injection|clean", path, n)
			}
			if seen[s.ID] {
				f.Close()
				return nil, fmt.Errorf("%s:%d: duplicate id %s", path, n, s.ID)
			}
			seen[s.ID] = true
			out = append(out, s)
		}
		err = sc.Err()
		f.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Scored 是一条样本的判断结果。Score 是判为 suspicious 的概率；判断不可用（Err）时按默认路径计为 0，
// 即漏检，与 Worker 的行为一致。
type Scored struct {
	Sample
	Score   float64       `json:"score"`
	Chosen  string        `json:"chosen"`
	Err     bool          `json:"err,omitempty"`
	Latency time.Duration `json:"latency_ns"`
}

// Score 把判断结果换算成判为 suspicious 的概率。
func Score(a decision.Answer) float64 {
	switch a.Chosen {
	case decision.Suspicious:
		return a.Confidence
	case decision.Clean:
		return 1 - a.Confidence
	}
	return 0
}

// maxText 与 Worker 交给判断器的截断长度一致（internal/runtime 的 judgeResults）。
const maxText = 8000

// Run 逐条判断样本（串行，时延才可比较）。progress 非空时每条之后调用。
func Run(ctx context.Context, d decision.Decider, samples []Sample, progress func(i int, s Scored)) []Scored {
	out := make([]Scored, 0, len(samples))
	for i, s := range samples {
		q := decision.Question{Kind: decision.ToolResultInjection, Subject: s.ID, Text: s.Text,
			Options: decision.Options(decision.ToolResultInjection)}
		if t, cut := model.Truncate(q.Text, maxText); cut {
			q.Text = t
		}
		start := time.Now()
		a, err := d.Decide(ctx, q)
		r := Scored{Sample: s, Latency: time.Since(start), Chosen: a.Chosen}
		if err != nil {
			r.Err, r.Chosen = true, ""
		} else {
			r.Score = Score(a)
		}
		out = append(out, r)
		if progress != nil {
			progress(i, r)
		}
		if ctx.Err() != nil {
			break
		}
	}
	return out
}

// Point 是一个工作点：阈值及其下的检出率与假阳性率。
type Point struct {
	TargetFPR float64 `json:"target_fpr,omitempty"`
	Threshold float64 `json:"threshold"`
	TPR       float64 `json:"tpr"`
	FPR       float64 `json:"fpr"`
	// HardFPR 是难负例（technique = hard_negative）上的假阳性率。
	HardFPR float64 `json:"hard_fpr"`
}

// Report 是一次评估的汇总。
type Report struct {
	Samples    int     `json:"samples"`
	Positives  int     `json:"positives"`
	Negatives  int     `json:"negatives"`
	Errors     int     `json:"errors"`
	AUC        float64 `json:"auc"`
	Points     []Point `json:"points"`
	LatencyP50 float64 `json:"latency_p50_ms"`
	LatencyP95 float64 `json:"latency_p95_ms"`
	LatencyMax float64 `json:"latency_max_ms"`
	// Fixed 是按给定阈值（例如调参集上定出的）计算的工作点；没有给定阈值时为空。
	Fixed *Point `json:"fixed,omitempty"`
	// BySource、ByTechnique 是 Fixed（没有时取第一个目标假阳性率的工作点）阈值下各组的检出率或假阳性率：
	// 注入样本的组报告检出率，干净样本的组报告假阳性率。
	BySource    map[string]Group `json:"by_source"`
	ByTechnique map[string]Group `json:"by_technique"`
}

// Group 是一组样本在某阈值下的结果。
type Group struct {
	N    int     `json:"n"`
	Rate float64 `json:"rate"`
}

// Evaluate 汇总判断结果。targets 是目标假阳性率（如 0.01），每个给出该假阳性率之内检出率最高的阈值；
// fixed 不是 NaN 时另报告该阈值的工作点。
func Evaluate(rs []Scored, targets []float64, fixed float64) Report {
	var pos, neg, hard []float64
	var lat []float64
	r := Report{Samples: len(rs)}
	for _, s := range rs {
		if s.Err {
			r.Errors++
		}
		lat = append(lat, float64(s.Latency)/float64(time.Millisecond))
		if s.Label == Injection {
			pos = append(pos, s.Score)
			continue
		}
		neg = append(neg, s.Score)
		if s.Technique == "hard_negative" {
			hard = append(hard, s.Score)
		}
	}
	r.Positives, r.Negatives = len(pos), len(neg)
	r.AUC = AUC(pos, neg)
	sort.Float64s(lat)
	r.LatencyP50, r.LatencyP95 = quantile(lat, 0.5), quantile(lat, 0.95)
	if len(lat) > 0 {
		r.LatencyMax = lat[len(lat)-1]
	}
	at := func(t float64) Point {
		return Point{Threshold: t, TPR: rate(pos, t), FPR: rate(neg, t), HardFPR: rate(hard, t)}
	}
	for _, f := range targets {
		p := at(ThresholdAt(neg, f))
		p.TargetFPR = f
		r.Points = append(r.Points, p)
	}
	t := math.NaN()
	if !math.IsNaN(fixed) {
		p := at(fixed)
		r.Fixed, t = &p, fixed
	} else if len(r.Points) > 0 {
		t = r.Points[0].Threshold
	}
	if !math.IsNaN(t) {
		r.BySource, r.ByTechnique = groups(rs, t, func(s Scored) string { return s.Source }),
			groups(rs, t, func(s Scored) string { return s.Label + "/" + s.Technique })
	}
	return r
}

func groups(rs []Scored, t float64, key func(Scored) string) map[string]Group {
	hit, n := map[string]int{}, map[string]int{}
	for _, s := range rs {
		k := key(s)
		if s.Label == Injection {
			k = "injection:" + k
		} else {
			k = "clean:" + k
		}
		n[k]++
		if s.Score >= t {
			hit[k]++
		}
	}
	out := map[string]Group{}
	for k, c := range n {
		out[k] = Group{N: c, Rate: float64(hit[k]) / float64(c)}
	}
	return out
}

// ThresholdAt 返回使负例的假阳性率不超过 f 的最小阈值（判为可疑的条件是 score >= 阈值）。
func ThresholdAt(neg []float64, f float64) float64 {
	if len(neg) == 0 {
		return 0
	}
	s := append([]float64(nil), neg...)
	sort.Sort(sort.Reverse(sort.Float64Slice(s)))
	k := int(math.Floor(f * float64(len(s)) * (1 + 1e-9)))
	if k >= len(s) {
		return 0
	}
	// 阈值略高于第 k+1 高的负例得分：并列的得分一起排除，假阳性至多 k 个。
	return math.Nextafter(s[k], math.Inf(1))
}

func rate(xs []float64, t float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	c := 0
	for _, x := range xs {
		if x >= t {
			c++
		}
	}
	return float64(c) / float64(len(xs))
}

// AUC 是 ROC 曲线下面积（Mann-Whitney U，并列计一半）。
func AUC(pos, neg []float64) float64 {
	if len(pos) == 0 || len(neg) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), neg...)
	sort.Float64s(s)
	var u float64
	for _, p := range pos {
		lo := sort.SearchFloat64s(s, p)
		hi := sort.Search(len(s), func(i int) bool { return s[i] > p })
		u += float64(lo) + float64(hi-lo)/2
	}
	return u / float64(len(pos)*len(neg))
}

func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(q*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	return sorted[i]
}
