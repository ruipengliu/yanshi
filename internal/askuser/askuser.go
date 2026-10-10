// Package askuser 是 Agent 生成的交互式提问（docs/design/m3-duplex-channel.md §7，ADR-0025）。
//
// 模型调用 ask_user 给出问题、可点选的选项与要填写的字段；它是一次路由到用户本人的调用：
// Worker 记下 ToolCallStarted（NodeID 为 NodeID）后挂起 Run，不经任何 Inbox。用户在任一设备上
// 点选、填写或直接输入文字，回答作为该调用的结果（外部结果，attempt = 0）写入日志，唤醒 Run。
// 其他设备从事件流看到结果，随之收起问题。
package askuser

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	v1 "yanshi/gen/yanshi/v1"
)

const (
	// Capability 是面向模型的工具名。
	Capability = "ask_user"
	// NodeID 是 ask_user 调用的路由目标：用户本人。不是 Node，不经 Inbox 投递。
	NodeID = "@user"
	// DefaultTimeout 是等待回答的上限：用户可能过很久才回来，挂起的 Run 不占用 Worker。
	DefaultTimeout = 24 * time.Hour

	MaxOptions = 8
	MaxFields  = 6
	// maxText 限制问题、选项、字段说明与回答文字的长度（字符）。
	maxText = 500
)

// Description 是给模型看的工具说明（改动须跑评测，见 AGENTS.md）。
const Description = "仅在无法继续时向用户提问：缺少必需的信息、且猜错会做错事，" +
	"例如要发送、修改或删除的对象有多个候选而用户没有指明。" +
	"分析、解读、写作、推荐类的请求不要提问：按合理的假设直接完成，并在回答中说明假设。" +
	"用户可以点选选项、填写字段或直接输入文字回答。" +
	"回答以 JSON 返回：selected 是选中的选项，values 是填写的字段，text 是用户输入的文字。"

// InputSchema 是 ask_user 参数的 JSON Schema。
const InputSchema = `{"type":"object","properties":{` +
	`"question":{"type":"string","description":"向用户提的问题"},` +
	`"options":{"type":"array","maxItems":8,"description":"可点选的选项","items":{"type":"object","properties":{` +
	`"id":{"type":"string"},"label":{"type":"string","description":"显示给用户的文字"},"description":{"type":"string"}},"required":["id","label"]}},` +
	`"multiple":{"type":"boolean","description":"是否允许多选，默认单选"},` +
	`"fields":{"type":"array","maxItems":6,"description":"需要用户填写的字段","items":{"type":"object","properties":{` +
	`"name":{"type":"string"},"label":{"type":"string"},"type":{"type":"string","enum":["text","number","date","time"]},` +
	`"required":{"type":"boolean"}},"required":["name","label"]}}},` +
	`"required":["question"]}`

type Option struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

type Field struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	// Type 为 text（默认）、number、date（YYYY-MM-DD）或 time（HH:MM）。
	Type     string `json:"type,omitempty"`
	Required bool   `json:"required,omitempty"`
}

// Question 是 ask_user 的参数。
type Question struct {
	Question string   `json:"question"`
	Options  []Option `json:"options,omitempty"`
	Multiple bool     `json:"multiple,omitempty"`
	Fields   []Field  `json:"fields,omitempty"`
}

// ErrInvalid 表示问题或回答不合法。
var ErrInvalid = errors.New("askuser: invalid")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

func tooLong(s string) bool { return utf8.RuneCountInString(s) > maxText }

// Parse 解析并校验模型给出的参数。不合法时的错误信息会作为调用结果交还模型，使其改正。
func Parse(args string) (*Question, error) {
	q := &Question{}
	if err := json.Unmarshal([]byte(args), q); err != nil {
		return nil, invalid("arguments are not valid JSON: %v", err)
	}
	if strings.TrimSpace(q.Question) == "" || tooLong(q.Question) {
		return nil, invalid("question is required and must be at most %d characters", maxText)
	}
	if len(q.Options) > MaxOptions || len(q.Fields) > MaxFields {
		return nil, invalid("at most %d options and %d fields", MaxOptions, MaxFields)
	}
	if len(q.Options) == 1 {
		return nil, invalid("give at least two options, or none")
	}
	seen := map[string]bool{}
	for _, o := range q.Options {
		if o.ID == "" || strings.TrimSpace(o.Label) == "" || seen[o.ID] || tooLong(o.Label) || tooLong(o.Description) {
			return nil, invalid("each option needs a unique id and a label of at most %d characters", maxText)
		}
		seen[o.ID] = true
	}
	seen = map[string]bool{}
	for i, f := range q.Fields {
		if f.Name == "" || strings.TrimSpace(f.Label) == "" || seen[f.Name] || tooLong(f.Label) {
			return nil, invalid("each field needs a unique name and a label")
		}
		seen[f.Name] = true
		switch f.Type {
		case "":
			q.Fields[i].Type = "text"
		case "text", "number", "date", "time":
		default:
			return nil, invalid("field %s: type must be text, number, date or time", f.Name)
		}
	}
	return q, nil
}

// Answer 是用户的回答：点选的选项 ID、填写的字段值与输入的文字，可以只有其中一部分。
type Answer struct {
	Selected []string          `json:"selected,omitempty"`
	Values   map[string]string `json:"values,omitempty"`
	Text     string            `json:"text,omitempty"`
}

var (
	dateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	timeRE = regexp.MustCompile(`^\d{2}:\d{2}$`)
)

// Check 校验回答是否符合问题。直接输入的文字总是可以作为回答（用户不想从选项里选）。
func (q *Question) Check(a *Answer) error {
	if tooLong(a.Text) {
		return invalid("text must be at most %d characters", maxText)
	}
	if len(a.Selected) > 1 && !q.Multiple {
		return invalid("only one option may be selected")
	}
	seen := map[string]bool{}
	for _, id := range a.Selected {
		if q.option(id) == nil || seen[id] {
			return invalid("unknown or repeated option %q", id)
		}
		seen[id] = true
	}
	for name, v := range a.Values {
		f := q.field(name)
		if f == nil {
			return invalid("unknown field %q", name)
		}
		if tooLong(v) {
			return invalid("field %s is too long", name)
		}
		if v == "" {
			continue
		}
		switch f.Type {
		case "number":
			if _, err := strconv.ParseFloat(v, 64); err != nil {
				return invalid("field %s must be a number", name)
			}
		case "date":
			if _, err := time.Parse(time.DateOnly, v); err != nil || !dateRE.MatchString(v) {
				return invalid("field %s must be a date (YYYY-MM-DD)", name)
			}
		case "time":
			if _, err := time.Parse("15:04", v); err != nil || !timeRE.MatchString(v) {
				return invalid("field %s must be a time (HH:MM)", name)
			}
		}
	}
	if strings.TrimSpace(a.Text) != "" {
		return nil
	}
	for _, f := range q.Fields {
		if f.Required && a.Values[f.Name] == "" {
			return invalid("field %s is required", f.Name)
		}
	}
	if len(a.Selected) == 0 && len(a.Values) == 0 {
		return invalid("empty answer")
	}
	return nil
}

func (q *Question) option(id string) *Option {
	for i := range q.Options {
		if q.Options[i].ID == id {
			return &q.Options[i]
		}
	}
	return nil
}

func (q *Question) field(name string) *Field {
	for i := range q.Fields {
		if q.Fields[i].Name == name {
			return &q.Fields[i]
		}
	}
	return nil
}

// Result 把回答渲染为调用结果交给模型：选中的选项带上标签，模型不必再对照 ID。
// extra 是用户随文字一并提交的其他内容块（如图片）。
func Result(q *Question, a *Answer, extra ...*v1.ContentBlock) []*v1.ContentBlock {
	type selected struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	}
	out := struct {
		Selected []selected        `json:"selected,omitempty"`
		Values   map[string]string `json:"values,omitempty"`
		Text     string            `json:"text,omitempty"`
	}{Text: a.Text}
	for _, id := range a.Selected {
		if o := q.option(id); o != nil {
			out.Selected = append(out.Selected, selected{ID: id, Label: o.Label})
		}
	}
	for k, v := range a.Values {
		if v != "" {
			if out.Values == nil {
				out.Values = map[string]string{}
			}
			out.Values[k] = v
		}
	}
	b, _ := json.Marshal(out)
	blocks := []*v1.ContentBlock{{Kind: &v1.ContentBlock_Text{Text: &v1.Text{Text: string(b)}}}}
	return append(blocks, extra...)
}

// Moderated 是回答中需要内容安全检查的文字：输入的文字与填写的值（选项是模型给出的，已在输出时检查）。
func (a *Answer) Moderated() string {
	parts := []string{a.Text}
	for _, v := range a.Values {
		parts = append(parts, v)
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}
