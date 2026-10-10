package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/agentdef"
	"yanshi/internal/eval"
	"yanshi/internal/model"
	sandboxdocker "yanshi/internal/sandbox/docker"
)

// evalCmd 运行评测集并与基线比较（docs/design/m4-eval.md）；出现回归时以非零状态退出。
func evalCmd(args []string) error {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	suite := fs.String("suite", "evals", "评测用例目录")
	agentsDir := fs.String("agents", "agents", "AgentDef 目录")
	caseGlob := fs.String("case", "*", "只运行名称匹配的用例")
	trials := fs.Int("trials", 0, "覆盖每个用例的运行次数")
	agentVersion := fs.String("agent-version", "", "评测该 AgentDef 版本而不是稳定版本（灰度前的评测门槛）")
	modelOverride := fs.String("model", "", "替换被评测 AgentDef 的模型（provider/model），用于比较模型")
	judgeModel := fs.String("judge", envOr("YANSHI_JUDGE_MODEL", "tokenhub/glm-5.3"), "评分模型（与被评测模型不同系列，ADR-0018）")
	embedModel := fs.String("embedding-model", os.Getenv("YANSHI_EMBEDDING_MODEL"), "Memory 检索用的嵌入模型")
	sandboxKind := fs.String("sandbox", "none", "代码沙箱：none | docker（none 时跳过需要沙箱的用例）")
	sandboxImage := fs.String("sandbox-image", "yanshi-sandbox:dev", "沙箱镜像")
	parallel := fs.Int("parallel", 4, "同时运行的次数")
	baselinePath := fs.String("baseline", "", "基线文件；默认 <suite>/baselines/<被评测模型或 default>.json")
	update := fs.Bool("update-baseline", false, "用本次结果更新基线（有意的效果变化，随代码提交）")
	outDir := fs.String("out", "eval-results", "报告输出目录")
	keepLogs := fs.Bool("logs", false, "同时保存每次运行的 Session 日志（<out>/<时间>/logs/），查看压缩摘要与调用过程")
	_ = fs.Parse(args)

	agents, err := agentdef.LoadDir(*agentsDir)
	if err != nil {
		return err
	}
	rel, _, err := agents.LoadReleases(filepath.Join(*agentsDir, agentdef.ReleasesFile))
	if err != nil {
		return err
	}
	agents.SetReleases(rel)
	all, err := eval.LoadSuite(*suite)
	if err != nil {
		return err
	}
	var cases []*eval.Case
	for _, c := range all {
		if ok, _ := path.Match(*caseGlob, c.Name); ok {
			cases = append(cases, c)
		}
	}
	if len(cases) == 0 {
		return fmt.Errorf("no cases match %q in %s", *caseGlob, *suite)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := eval.Config{Agents: agents, Model: gateway(logger), Judge: *judgeModel, EmbedModel: *embedModel,
		ModelOverride: *modelOverride, AgentVersion: *agentVersion, Trials: *trials, Parallel: *parallel, Logger: logger,
		Progress: func(name string, trial int, passed bool) {
			mark := "✓"
			if !passed {
				mark = "✗"
			}
			fmt.Fprintf(os.Stderr, "%s %s #%d\n", mark, name, trial+1)
		}}
	if *sandboxKind == "docker" {
		cfg.Sandbox = &sandboxdocker.Provider{Image: *sandboxImage}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	start := time.Now()
	dir := filepath.Join(*outDir, start.Format("20060102-150405"))
	if *keepLogs {
		cfg.LogDir = filepath.Join(dir, "logs")
	}
	rep, err := eval.Run(ctx, cfg, cases)
	if err != nil {
		return err
	}

	if *baselinePath == "" {
		name := "default"
		if *modelOverride != "" {
			name = regexp.MustCompile(`[^a-zA-Z0-9.-]+`).ReplaceAllString(*modelOverride, "_")
		}
		*baselinePath = filepath.Join(*suite, "baselines", name+".json")
	}
	baseline, err := eval.LoadReport(*baselinePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var regressions []string
	if baseline != nil {
		regressions = eval.Compare(baseline, rep)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	md := rep.Markdown(baseline, regressions)
	if err := rep.WriteJSON(filepath.Join(dir, "report.json")); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(md), 0o644); err != nil {
		return err
	}
	fmt.Println(md)
	fmt.Printf("报告：%s（%s）\n", dir, time.Since(start).Round(time.Second))
	if *update {
		if err := os.MkdirAll(filepath.Dir(*baselinePath), 0o755); err != nil {
			return err
		}
		// 只运行了部分用例（-case）时并入原基线，不丢失其余用例的基线。
		merged, skipped := eval.MergeBaseline(baseline, rep)
		if err := merged.WriteJSON(*baselinePath); err != nil {
			return err
		}
		fmt.Printf("已更新基线：%s\n", *baselinePath)
		if len(skipped) > 0 {
			fmt.Printf("以下用例因评测设施失败（如评分模型不可用）未写入基线，保留原值：%v\n", skipped)
		}
		return nil
	}
	if baseline == nil {
		fmt.Printf("没有基线（%s）；确认结果后用 -update-baseline 建立。\n", *baselinePath)
	}
	if len(regressions) > 0 {
		return fmt.Errorf("%d regression(s) against %s", len(regressions), *baselinePath)
	}
	return nil
}

// exportCaseCmd 把一个 Session 的用户输入导出为评测用例骨架（docs/design/m4-eval.md §5）。
// 骨架需要人工补充断言，并在取得用户同意、完成脱敏后才能加入评测集。
func exportCaseCmd(args []string) error {
	fs := flag.NewFlagSet("export-case", flag.ExitOnError)
	server := fs.String("server", "http://127.0.0.1:8080", "yanshi 服务地址")
	sid := fs.String("session", "", "Session ID")
	name := fs.String("name", "exported", "用例名")
	tf := addTokenFlags(fs)
	_ = fs.Parse(args)
	if *sid == "" {
		return errors.New("-session is required")
	}
	// 导出由业务线服务端执行：-key 时签发服务令牌（不带 EndUser）。
	token, _, err := tf.source("", "")
	if err != nil {
		return err
	}
	req, _ := http.NewRequest(http.MethodGet, strings.TrimRight(*server, "/")+"/v1/sessions/"+*sid+"/events", nil)
	if token != nil {
		t, err := token(context.Background())
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("export: %s %s", resp.Status, b)
	}
	var body struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return err
	}
	fmt.Printf("# 由 Session %s 导出的用例骨架。加入评测集之前：\n#   1. 确认已取得用户同意，并删除或替换全部个人信息（姓名、联系方式、地址等）；\n#   2. 为每一轮补充 expect 断言；设备文件按虚拟设备的格式另行准备（setup.device.files）。\n", *sid)
	fmt.Printf("name: %s\ndescription: \"\"\ntrials: 3\nturns:\n", *name)
	for _, raw := range body.Events {
		e := &v1.Event{}
		if protojson.Unmarshal(raw, e) != nil {
			continue
		}
		var input []*v1.ContentBlock
		switch p := e.GetPayload().(type) {
		case *v1.Event_RunRequested:
			input = p.RunRequested.GetInput()
		case *v1.Event_Steered:
			input = p.Steered.GetInput()
		default:
			continue
		}
		q, _ := json.Marshal(model.Text(input))
		fmt.Printf("  - input: %s\n    expect: {}\n", q)
	}
	return nil
}
