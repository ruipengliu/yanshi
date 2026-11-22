package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"time"

	"yanshi/internal/decision"
	"yanshi/internal/decision/offline"
	"yanshi/internal/model"
)

// decisionEvalCmd 在固定样本集上离线评估注入判断（docs/design/m6-decisions.md §9）。与 serve 的 -injection-check
// 用同一说明与同一 Logits，但不设置信度退路，得分是模型给 suspicious 的概率；时限与 serve 相同（Guarded），
// 超时计为漏检。
func decisionEvalCmd(args []string) error {
	fs := flag.NewFlagSet("decision-eval", flag.ExitOnError)
	check := fs.String("check", "rules", "判断器：rules | <provider/model>（返回 logprobs 的端点，如 local/qwen3-4b）")
	data := fs.String("data", "evals/decisions/injection/tune.jsonl", "样本文件（JSONL），逗号分隔可给多个；阈值只在调参集上确定，留出集只报告最终数字")
	targets := fs.String("fpr", "0.005,0.01,0.02,0.05", "目标假阳性率，逗号分隔")
	threshold := fs.String("threshold", "", "固定阈值（通常取调参集上定出的）；报告该阈值下的工作点")
	timeout := fs.Duration("timeout", 2*time.Second, "每条的时限，与 serve 相同")
	out := fs.String("out", "", "把报告与逐条得分写成 JSON")
	fs.Parse(args)

	var fpr []float64
	for _, f := range strings.Split(*targets, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		v, err := strconv.ParseFloat(f, 64)
		if err != nil {
			return fmt.Errorf("-fpr %q: %w", f, err)
		}
		fpr = append(fpr, v)
	}
	fixed := math.NaN()
	if *threshold != "" {
		v, err := strconv.ParseFloat(*threshold, 64)
		if err != nil {
			return fmt.Errorf("-threshold: %w", err)
		}
		fixed = v
	}
	samples, err := offline.Load(strings.Split(*data, ",")...)
	if err != nil {
		return err
	}
	var d decision.Decider
	switch {
	case *check == "rules":
		d = decision.InjectionRules{}
	case strings.Contains(*check, "/"):
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		gw := gateway(logger, model.Limits{}, nil, nil)
		d = decision.Guarded{Timeout: *timeout, Decider: &decision.Logits{Provider: gw, Model: *check,
			Prompts: map[string]string{decision.ToolResultInjection: injectionPrompt}}}
	default:
		return fmt.Errorf("-check %q: want rules or provider/model", *check)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	rs := offline.Run(ctx, d, samples, func(i int, s offline.Scored) {
		if (i+1)%50 == 0 {
			fmt.Fprintf(os.Stderr, "%d/%d\n", i+1, len(samples))
		}
	})
	r := offline.Evaluate(rs, fpr, fixed)
	printDecisionReport(os.Stdout, *check, r)
	if *out != "" {
		b, err := json.MarshalIndent(struct {
			Check   string           `json:"check"`
			Data    string           `json:"data"`
			Report  offline.Report   `json:"report"`
			Results []offline.Scored `json:"results"`
		}{*check, *data, r, rs}, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(*out, b, 0o644)
	}
	return nil
}

func printDecisionReport(w io.Writer, check string, r offline.Report) {
	fmt.Fprintf(w, "判断器 %s：样本 %d（注入 %d，干净 %d），不可用 %d\n", check, r.Samples, r.Positives, r.Negatives, r.Errors)
	fmt.Fprintf(w, "AUC %.3f；时延 p50 %.0f ms，p95 %.0f ms，最大 %.0f ms\n", r.AUC, r.LatencyP50, r.LatencyP95, r.LatencyMax)
	fmt.Fprintln(w, "目标假阳性率  阈值      检出率  假阳性率  难负例假阳性率")
	for _, p := range r.Points {
		fmt.Fprintf(w, "%-12.3f  %-8.4f  %5.1f%%  %6.1f%%  %6.1f%%\n", p.TargetFPR, p.Threshold, 100*p.TPR, 100*p.FPR, 100*p.HardFPR)
	}
	if p := r.Fixed; p != nil {
		fmt.Fprintf(w, "固定阈值      %-8.4f  %5.1f%%  %6.1f%%  %6.1f%%\n", p.Threshold, 100*p.TPR, 100*p.FPR, 100*p.HardFPR)
	}
	printGroups(w, "按来源", r.BySource)
	printGroups(w, "按手法", r.ByTechnique)
}

func printGroups(w io.Writer, title string, g map[string]offline.Group) {
	if len(g) == 0 {
		return
	}
	keys := make([]string, 0, len(g))
	for k := range g {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprintf(w, "%s（注入组为检出率，干净组为假阳性率）：\n", title)
	for _, k := range keys {
		fmt.Fprintf(w, "  %-40s n=%-4d %5.1f%%\n", k, g[k].N, 100*g[k].Rate)
	}
}
