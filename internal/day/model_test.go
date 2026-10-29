package day

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/model"
	"yanshi/internal/model/script"
)

// 专用脚本只读模型实际收到的工具结果；不能通过业务库替模型补齐引用、ID 或 rev。
// A10 的固定指令不含脚本命令，因此不修改控制台的通用 script.Provider。
type dayModel struct{ trace *scriptTrace }
type toolResult struct {
	name, args string
	blocks     []*v1.ContentBlock
}

func currentTools(req *model.Request) (string, []toolResult) {
	start, input := 0, ""
	for i, m := range req.Messages {
		if m.Role == model.RoleUser {
			start, input = i, model.Text(m.Content)
		}
	}
	names := map[string]string{}
	arguments := map[string]string{}
	var results []toolResult
	for _, m := range req.Messages[start:] {
		for _, call := range m.ToolCalls {
			names[call.GetCallId()] = call.GetCapability()
			arguments[call.GetCallId()] = call.GetArgumentsJson()
		}
		if m.Role == model.RoleTool {
			results = append(results, toolResult{names[m.ToolCallID], arguments[m.ToolCallID], m.Content})
		}
	}
	return input, results
}
func callTool(req *model.Request, suffix string, args any) (*model.Response, error) {
	name := ""
	for _, tool := range req.Tools {
		if tool.Name == suffix || strings.HasSuffix(tool.Name, "__"+suffix) {
			name = tool.Name
			break
		}
	}
	if name == "" {
		return nil, fmt.Errorf("day script: missing tool %s", suffix)
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	return &model.Response{Model: req.Model, FinishReason: "tool_calls", ToolCalls: []*v1.ToolCall{{CallId: "script", Capability: name, ArgumentsJson: string(raw)}}, Usage: &v1.Usage{InputTokens: 1, OutputTokens: 1}}, nil
}
func done(req *model.Request) (*model.Response, error) {
	return &model.Response{Model: req.Model, FinishReason: "stop", Content: model.TextBlocks("整理完成"), Usage: &v1.Usage{InputTokens: 1, OutputTokens: 1}}, nil
}

var afterPattern = regexp.MustCompile(`after="([^"]+)"`)
var commitmentPattern = regexp.MustCompile(`(?s)(C-[0-9]+)[^\n]*?rev\s*([0-9]+)`)
var projectPattern = regexp.MustCompile(`P-[0-9]+`)

type scriptCapture struct{ ref, text string }

var captureLinePattern = regexp.MustCompile(`(?m)^#([0-9]+\.[0-9]+) (?:[0-9]{2}:[0-9]{2} )?\[(?:佩戴者|他人[^\]]*)\] (.*)$`)

// Provider 只收到 Transcript 展开的文字；不能读取日志里的结构化 Capture 或从业务库补页。
func scriptCaptures(text string) ([]scriptCapture, error) {
	if strings.HasPrefix(text, "[error]") || strings.Contains(text, "[已截断：") {
		return nil, fmt.Errorf("day script: unusable capture page")
	}
	var out []scriptCapture
	for _, match := range captureLinePattern.FindAllStringSubmatch(text, -1) {
		out = append(out, scriptCapture{match[1], match[2]})
	}
	if len(out) == 0 && !strings.HasPrefix(text, "（已读完：") && !(strings.HasPrefix(text, "（时段 #") && strings.Contains(text, "还在进行")) {
		return nil, fmt.Errorf("day script: unrecognized capture page")
	}
	return out, nil
}

func (d dayModel) Generate(ctx context.Context, req *model.Request, delta func(model.Delta)) (response *model.Response, err error) {
	if d.trace != nil {
		defer func() { d.trace.record(req, response, err) }()
	}
	input, results := currentTools(req)
	if req.Model == "xinli-assistant" {
		return (script.Provider{}).Generate(ctx, req, delta)
	}
	if len(results) > 0 && strings.HasPrefix(model.Text(results[len(results)-1].blocks), "[error]") {
		return nil, fmt.Errorf("day script: tool failed")
	}
	last := ""
	if len(results) > 0 {
		last = results[len(results)-1].name
	}
	if req.Model == "xinli-week" {
		if len(results) == 0 {
			return callTool(req, "week_overview", map[string]any{})
		}
		if strings.HasSuffix(last, "__week_overview") {
			return callTool(req, "file_week", map[string]any{"habits": []any{}, "suggestions": []string{}})
		}
		return done(req)
	}
	if req.Model == "xinli-project" {
		if len(results) == 0 {
			if strings.Contains(input, "重写") {
				return callTool(req, "project_digest", map[string]any{})
			}
			date := regexp.MustCompile(`[0-9]{4}-[0-9]{2}-[0-9]{2}`).FindString(input)
			if date == "" {
				return nil, fmt.Errorf("day script: project input has no date")
			}
			return callTool(req, "day_summary", map[string]string{"date": date})
		}
		if strings.HasSuffix(last, "__day_summary") || strings.HasSuffix(last, "__project_digest") {
			return callTool(req, "set_status", map[string]any{"status": "已确认安排，等待执行", "next_steps": []string{"跟进承诺"}, "open_questions": []string{}})
		}
		return done(req)
	}
	all := strings.Contains(input, "整理今天")
	finishDay := func() (*model.Response, error) {
		if !all {
			return done(req)
		}
		return callTool(req, "day_ledger", map[string]any{})
	}
	if len(results) == 0 {
		return callTool(req, "captures", map[string]any{"after": "new", "all": all})
	}
	var page []scriptCapture
	pageText, related := "", ""
	for _, result := range results {
		if strings.HasSuffix(result.name, "__captures") {
			text := model.Text(result.blocks)
			// 续页仍属于同一时段，只有交出成功后下一次 captures 才换时段。
			if !strings.Contains(text, "（续）") {
				page = nil
			}
			pageText = text
			segments, err := scriptCaptures(text)
			if err != nil {
				return nil, err
			}
			page = append(page, segments...)
		}
		if strings.HasSuffix(result.name, "__find_related") {
			related = model.Text(result.blocks)
		}
	}
	switch {
	case strings.HasSuffix(last, "__captures"):
		if strings.Contains(pageText, "还在进行") {
			return done(req)
		}
		if strings.Contains(pageText, "起未完") {
			m := afterPattern.FindStringSubmatch(pageText)
			if len(m) != 2 {
				return nil, fmt.Errorf("day script: missing continuation")
			}
			return callTool(req, "captures", map[string]any{"after": m[1], "all": all})
		}
		if len(page) == 0 {
			return finishDay()
		}
		return callTool(req, "find_related", map[string]any{"query": "报价", "counterparts": []string{"张总"}})
	case strings.HasSuffix(last, "__find_related"):
		if len(page) == 0 {
			return nil, fmt.Errorf("day script: no attested captures")
		}
		exchanges, create, revise := []any{}, []any{}, []any{}
		for _, segment := range page {
			ref := "#" + segment.ref
			if strings.Contains(segment.text, "闲聊") {
				continue
			}
			name, content, counterpart := "办公室装修", "发报价单", "张总"
			if strings.Contains(segment.text, "预算") {
				name, content, counterpart = "部门预算", "发预算表", "李总"
			}
			projects := []any{map[string]string{"match": "new", "name": name}}
			if strings.Contains(segment.text, "改到") {
				m := commitmentPattern.FindStringSubmatch(related)
				project := ""
				for _, line := range strings.Split(related, "\n") {
					if len(m) == 3 && strings.Contains(line, m[1]) {
						project = projectPattern.FindString(line)
						break
					}
				}
				if len(m) != 3 || project == "" {
					return nil, fmt.Errorf("day script: related result lacks commitment/project identity")
				}
				rev, err := strconv.Atoi(m[2])
				if err != nil {
					return nil, err
				}
				revise = append(revise, map[string]any{"id": m[1], "rev": rev, "due_text": "后天", "refs": []string{ref}, "reason": "双方确认改期"})
				projects = []any{map[string]string{"match": "existing", "id": project}}
			} else if !strings.Contains(segment.text, "闲聊") {
				create = append(create, map[string]any{"exchange": "x" + segment.ref, "kind": "mine", "content": content, "counterpart": counterpart, "due_text": "明天", "refs": []string{ref}})
			}
			exchanges = append(exchanges, map[string]any{"from": ref, "to": ref, "title": content, "summary": content, "projects": projects})
		}
		return callTool(req, "file_changes", map[string]any{"source": map[string]string{"from": "#" + page[0].ref, "to": "#" + page[len(page)-1].ref},
			"exchanges": map[string]any{"create": exchanges}, "commitments": map[string]any{"create": create, "revise": revise}})
	case strings.HasSuffix(last, "__file_changes"):
		if strings.Contains(model.Text(results[len(results)-1].blocks), "没有被接受") {
			// 擦除后同 source 重交会先移除旧变更集；只按回执给出的基线 rev 重交完整参数。
			lastResult := results[len(results)-1]
			matches := regexp.MustCompile(`(C-[0-9]+) 的 rev 是 ([0-9]+) 而不是`).FindAllStringSubmatch(model.Text(lastResult.blocks), -1)
			if len(matches) == 0 {
				return nil, fmt.Errorf("day script: changes rejected")
			}
			var args map[string]any
			if err := json.Unmarshal([]byte(lastResult.args), &args); err != nil {
				return nil, err
			}
			ops, ok := args["commitments"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("day script: rejected call lacks commitments")
			}
			items, _ := ops["revise"].([]any)
			changed := false
			for _, item := range items {
				change, ok := item.(map[string]any)
				if !ok {
					continue
				}
				for _, m := range matches {
					if change["id"] == m[1] {
						rev, err := strconv.Atoi(m[2])
						if err != nil {
							return nil, err
						}
						if change["rev"] != float64(rev) {
							change["rev"] = rev
							changed = true
						}
					}
				}
			}
			if !changed {
				return nil, fmt.Errorf("day script: rejected revision did not advance")
			}
			return callTool(req, "file_changes", args)
		}
		if strings.Contains(pageText, "已读完") {
			return finishDay()
		}
		m := afterPattern.FindStringSubmatch(pageText)
		if len(m) != 2 {
			return nil, fmt.Errorf("day script: missing page cursor")
		}
		return callTool(req, "captures", map[string]any{"after": m[1], "all": all})
	case strings.HasSuffix(last, "__day_ledger"):
		return callTool(req, "file_day", map[string]any{"day": map[string]any{"headline": "报价与预算安排已整理", "highlights": []string{"确认两项安排"}}})
	case strings.HasSuffix(last, "__file_day"):
		return done(req)
	default:
		return nil, fmt.Errorf("day script: unexpected tool result")
	}
}
