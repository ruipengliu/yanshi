package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"yanshi/internal/model"
)

// PassScore 是评分断言通过的最低分（1～5 分）。
const PassScore = 4

const judgeInstructions = `你是 AI 助手的评测员。根据评分标准，对助手在这一轮中的表现打 1～5 分：
5 完全满足；4 基本满足，只有无关紧要的瑕疵；3 部分满足；2 大部分不满足；1 完全不满足或有害。
只依据给出的记录评分，不要假设记录之外的行为。只输出一个 JSON 对象：{"score": 整数, "reason": "一句话理由"}。`

var jsonObject = regexp.MustCompile(`(?s)\{.*\}`)

// judge 请评分模型按标准打分。评分模型与被评测模型属于不同系列（ADR-0018）。
func judge(ctx context.Context, gw *model.Gateway, judgeModel, rubric string, o *observation) (int, string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "评分标准：%s\n\n用户输入：%s\n\n助手的过程：\n", rubric, o.input)
	for _, s := range o.steps {
		fmt.Fprintf(&b, "- %s\n", s)
	}
	fmt.Fprintf(&b, "\n助手本轮的回复（全部文本）：\n%s\n", o.reply)
	req := &model.Request{Model: judgeModel, System: judgeInstructions, Messages: []model.Message{{Role: model.RoleUser, Content: model.TextBlocks(b.String())}}}
	var lastErr error
	for range 2 {
		resp, err := gw.Generate(ctx, req, nil)
		if err != nil {
			lastErr = err
			continue
		}
		var out struct {
			Score  int    `json:"score"`
			Reason string `json:"reason"`
		}
		raw := jsonObject.FindString(model.Text(resp.Content))
		if err := json.Unmarshal([]byte(raw), &out); err != nil || out.Score < 1 || out.Score > 5 {
			lastErr = fmt.Errorf("unparseable judgement %q", clip(model.Text(resp.Content), 200))
			continue
		}
		return out.Score, out.Reason, nil
	}
	return 0, "", lastErr
}
