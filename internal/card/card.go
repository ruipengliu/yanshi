// Package card 是 Agent 展示给用户的卡片（docs/design/cards.md，ADR-0032）。
//
// 模型调用内置工具 show_card 给出卡片；平台校验后把规范化的卡片（v1.Card）作为调用结果写入日志，客户端据此渲染。
// 卡片不阻塞 Run。用户点按钮、点选列表项时，客户端提交一条带 CardAction 的输入：平台按日志中的卡片核对动作与
// 字段、填写动作的显示文字，以 call_id 与动作推导输入 ID（同一版卡片上的同一动作只生效一次），再像普通输入一样
// 并入进行中的 Run 或开启新 Run。
package card

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/artifact"
	"yanshi/internal/capability"
	"yanshi/internal/model"
)

// Capability 是面向模型的工具名。
const Capability = "show_card"

// 卡片的上限：卡片随结果写入日志并推送给每台设备，也会被模型在后续请求中看到摘要。
const (
	MaxElements = 20
	MaxItems    = 20
	MaxFields   = 8
	MaxButtons  = 6
	maxTitle    = 100
	maxText     = 4000
	maxShort    = 200
)

// Description 是给模型看的工具说明（改动须跑评测，见 AGENTS.md）。
const Description = "在对话中展示一张卡片：图文混排的结果（标题、Markdown 正文、图片、键值、列表），可带按钮与待填写的字段。" +
	"适合展示结构化结果（日程、行程、订单、对比、操作确认后的状态），以及让用户在多个后续操作中点选。" +
	"卡片不会等待用户：展示后继续完成你的工作；用户之后点按钮或点选列表项时，你会收到一条卡片动作输入。" +
	"必须等用户回答才能继续时用 ask_user，不要用卡片。" +
	"同一 card_id 再次展示会替换之前的卡片（如把\"正在设置\"更新为\"已设置\"）。" +
	"图片只能用本会话的 artifact://<id>（用户上传、设备或沙箱产出的文件）或内置图标 icon:<名称>。" +
	"展示卡片后不必在回复中重复卡片里的内容，简短说明即可。"

// Icons 是内置图标（客户端负责渲染）。
var Icons = []string{"alarm", "calendar", "car", "check", "clock", "cloud", "file", "heart", "home", "info", "location",
	"mail", "message", "music", "phone", "photo", "search", "shopping", "star", "timer", "user", "warning", "weather"}

// InputSchema 是 show_card 参数的 JSON Schema。body 中每个元素以 type 区分，比嵌套的 oneof 更容易让模型写对。
var InputSchema = `{"type":"object","properties":{` +
	`"card_id":{"type":"string","description":"卡片 ID（字母、数字、_、-）；省略时自动生成；用已有 ID 再次展示即替换该卡片"},` +
	`"title":{"type":"string"},"subtitle":{"type":"string"},` +
	`"image":{"type":"string","description":"头图：artifact://<id> 或 icon:<名称>"},` +
	`"body":{"type":"array","maxItems":20,"items":{"type":"object","properties":{` +
	`"type":{"type":"string","enum":["markdown","image","facts","list","field"]},` +
	`"text":{"type":"string","description":"markdown 的内容"},` +
	`"uri":{"type":"string","description":"image 的地址：artifact://<id> 或 icon:<名称>"},"alt":{"type":"string"},` +
	`"facts":{"type":"array","items":{"type":"object","properties":{"label":{"type":"string"},"value":{"type":"string"}},"required":["label","value"]}},` +
	`"items":{"type":"array","description":"list 的条目；带 action 的条目可点选","items":{"type":"object","properties":{` +
	`"title":{"type":"string"},"subtitle":{"type":"string"},"image":{"type":"string"},"action":{"type":"string","description":"点选该条目触发的动作 ID"}},"required":["title"]}},` +
	`"id":{"type":"string","description":"field 的 ID"},"label":{"type":"string","description":"field 的说明"},` +
	`"input":{"type":"string","enum":["text","number","date","time","select","toggle"],"description":"field 的类型"},` +
	`"options":{"type":"array","items":{"type":"string"},"description":"select 的选项"},` +
	`"value":{"type":"string","description":"field 的初始值"},"required":{"type":"boolean"}},` +
	`"required":["type"]}},` +
	`"buttons":{"type":"array","maxItems":6,"items":{"type":"object","properties":{` +
	`"id":{"type":"string","description":"动作 ID"},"label":{"type":"string"},"style":{"type":"string","enum":["primary","default","danger"]}},` +
	`"required":["id","label"]}}},` +
	`"required":["title"]}`

// ErrInvalid 表示卡片或卡片动作不合法。
var ErrInvalid = errors.New("card: invalid")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

type wireElement struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	URI     string `json:"uri"`
	Alt     string `json:"alt"`
	Facts   []struct{ Label, Value string }
	Items   []struct{ Title, Subtitle, Image, Action string }
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	Input   string   `json:"input"`
	Options []string `json:"options"`
	Value   string   `json:"value"`
	Req     bool     `json:"required"`
}

type wireCard struct {
	CardID   string        `json:"card_id"`
	Title    string        `json:"title"`
	Subtitle string        `json:"subtitle"`
	Image    string        `json:"image"`
	Body     []wireElement `json:"body"`
	Buttons  []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		Style string `json:"style"`
	} `json:"buttons"`
}

// ImageCheck 核对卡片中引用的工件属于本 Session；为 nil 时不允许引用工件。
type ImageCheck func(ctx context.Context, artifactID string) error

// Parse 校验 show_card 的参数并返回规范化的卡片。callID 是这次调用：卡片 ID 省略时由它生成。
func Parse(ctx context.Context, args, callID string, check ImageCheck) (*v1.Card, error) {
	var w wireCard
	if err := json.Unmarshal([]byte(args), &w); err != nil {
		return nil, invalid("arguments are not a JSON object: %v", err)
	}
	c := &v1.Card{CardId: w.CardID, CallId: callID, Title: strings.TrimSpace(w.Title), Subtitle: strings.TrimSpace(w.Subtitle), Image: w.Image}
	if c.CardId == "" {
		c.CardId = "card_" + shortHash(callID)
	}
	if !idPattern.MatchString(c.CardId) {
		return nil, invalid("card_id must match %s", idPattern)
	}
	if c.Title == "" || tooLong(c.Title, maxTitle) || tooLong(c.Subtitle, maxShort) {
		return nil, invalid("title is required; title ≤ %d and subtitle ≤ %d characters", maxTitle, maxShort)
	}
	if err := checkImage(ctx, c.Image, check); err != nil {
		return nil, err
	}
	if len(w.Body) > MaxElements {
		return nil, invalid("at most %d body elements", MaxElements)
	}
	actions := map[string]bool{}
	addAction := func(id string) error {
		if !idPattern.MatchString(id) {
			return invalid("action id %q must match %s", id, idPattern)
		}
		if actions[id] {
			return invalid("duplicate action id %q", id)
		}
		actions[id] = true
		return nil
	}
	fields := map[string]bool{}
	for i, e := range w.Body {
		el, err := element(ctx, e, check, addAction, fields)
		if err != nil {
			return nil, fmt.Errorf("body[%d]: %w", i, err)
		}
		c.Body = append(c.Body, el)
	}
	if len(fields) > MaxFields {
		return nil, invalid("at most %d fields", MaxFields)
	}
	if len(w.Buttons) > MaxButtons {
		return nil, invalid("at most %d buttons", MaxButtons)
	}
	for _, b := range w.Buttons {
		if err := addAction(b.ID); err != nil {
			return nil, err
		}
		label := strings.TrimSpace(b.Label)
		if label == "" || tooLong(label, maxShort) {
			return nil, invalid("button %s: label is required (≤ %d characters)", b.ID, maxShort)
		}
		style := b.Style
		if style != "" && style != "primary" && style != "default" && style != "danger" {
			return nil, invalid("button %s: style must be primary, default or danger", b.ID)
		}
		c.Buttons = append(c.Buttons, &v1.CardButton{Id: b.ID, Label: label, Style: style})
	}
	if len(fields) > 0 && len(c.Buttons) == 0 {
		return nil, invalid("fields are submitted with a button: add one")
	}
	return c, nil
}

func element(ctx context.Context, e wireElement, check ImageCheck, addAction func(string) error, fields map[string]bool) (*v1.CardElement, error) {
	switch e.Type {
	case "markdown":
		if strings.TrimSpace(e.Text) == "" || tooLong(e.Text, maxText) {
			return nil, invalid("markdown: text is required (≤ %d characters)", maxText)
		}
		return &v1.CardElement{Kind: &v1.CardElement_Markdown{Markdown: &v1.CardMarkdown{Text: e.Text}}}, nil
	case "image":
		if e.URI == "" {
			return nil, invalid("image: uri is required")
		}
		if err := checkImage(ctx, e.URI, check); err != nil {
			return nil, err
		}
		return &v1.CardElement{Kind: &v1.CardElement_Image{Image: &v1.CardImage{Uri: e.URI, Alt: truncate(e.Alt, maxShort)}}}, nil
	case "facts":
		if len(e.Facts) == 0 || len(e.Facts) > MaxItems {
			return nil, invalid("facts: 1 to %d entries", MaxItems)
		}
		f := &v1.CardFacts{}
		for _, kv := range e.Facts {
			if strings.TrimSpace(kv.Label) == "" || tooLong(kv.Label, maxShort) || tooLong(kv.Value, maxShort) {
				return nil, invalid("facts: label is required; label and value ≤ %d characters", maxShort)
			}
			f.Facts = append(f.Facts, &v1.CardFact{Label: kv.Label, Value: kv.Value})
		}
		return &v1.CardElement{Kind: &v1.CardElement_Facts{Facts: f}}, nil
	case "list":
		if len(e.Items) == 0 || len(e.Items) > MaxItems {
			return nil, invalid("list: 1 to %d items", MaxItems)
		}
		l := &v1.CardList{}
		for _, it := range e.Items {
			if strings.TrimSpace(it.Title) == "" || tooLong(it.Title, maxShort) || tooLong(it.Subtitle, maxShort) {
				return nil, invalid("list: title is required; title and subtitle ≤ %d characters", maxShort)
			}
			if err := checkImage(ctx, it.Image, check); err != nil {
				return nil, err
			}
			if it.Action != "" {
				if err := addAction(it.Action); err != nil {
					return nil, err
				}
			}
			l.Items = append(l.Items, &v1.CardListItem{Title: it.Title, Subtitle: it.Subtitle, Image: it.Image, Action: it.Action})
		}
		return &v1.CardElement{Kind: &v1.CardElement_List{List: l}}, nil
	case "field":
		if !idPattern.MatchString(e.ID) || fields[e.ID] {
			return nil, invalid("field: id must be unique and match %s", idPattern)
		}
		fields[e.ID] = true
		if strings.TrimSpace(e.Label) == "" || tooLong(e.Label, maxShort) {
			return nil, invalid("field %s: label is required (≤ %d characters)", e.ID, maxShort)
		}
		f := &v1.CardField{Id: e.ID, Label: e.Label, Type: e.Input, Value: e.Value, Required: e.Req}
		switch e.Input {
		case "", "text", "number", "date", "time", "toggle":
		case "select":
			if len(e.Options) == 0 || len(e.Options) > MaxItems {
				return nil, invalid("field %s: select needs 1 to %d options", e.ID, MaxItems)
			}
			f.Options = e.Options
		default:
			return nil, invalid("field %s: input must be text, number, date, time, select or toggle", e.ID)
		}
		if err := checkValue(f, e.Value); e.Value != "" && err != nil {
			return nil, err
		}
		return &v1.CardElement{Kind: &v1.CardElement_Field{Field: f}}, nil
	default:
		return nil, invalid("type must be markdown, image, facts, list or field")
	}
}

// checkImage 只允许本 Session 的工件与内置图标。
func checkImage(ctx context.Context, uri string, check ImageCheck) error {
	if uri == "" {
		return nil
	}
	if name, ok := strings.CutPrefix(uri, "icon:"); ok {
		if !slices.Contains(Icons, name) {
			return invalid("unknown icon %q; available: %s", name, strings.Join(Icons, ", "))
		}
		return nil
	}
	id, ok := artifact.ParseURI(uri)
	if !ok {
		return invalid("image %q: only artifact://<id> of this session or icon:<name> are allowed", uri)
	}
	if check == nil {
		return invalid("image %q: artifacts are not available", uri)
	}
	if err := check(ctx, id); err != nil {
		return invalid("image %q: no such artifact in this session", uri)
	}
	return nil
}

var (
	datePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	timePattern = regexp.MustCompile(`^\d{2}:\d{2}$`)
)

func checkValue(f *v1.CardField, v string) error {
	if tooLong(v, maxText) {
		return invalid("field %s: value too long", f.GetId())
	}
	switch f.GetType() {
	case "number":
		if _, err := strconv.ParseFloat(v, 64); err != nil {
			return invalid("field %s: not a number", f.GetId())
		}
	case "date":
		if !datePattern.MatchString(v) {
			return invalid("field %s: date must be YYYY-MM-DD", f.GetId())
		}
	case "time":
		if !timePattern.MatchString(v) {
			return invalid("field %s: time must be HH:MM", f.GetId())
		}
	case "select":
		if !slices.Contains(f.GetOptions(), v) {
			return invalid("field %s: not one of the options", f.GetId())
		}
	case "toggle":
		if v != "true" && v != "false" {
			return invalid("field %s: toggle must be true or false", f.GetId())
		}
	}
	return nil
}

// CheckAction 按卡片核对动作：动作须是卡片上的按钮或可点选的列表项，字段只能是卡片声明过的、必填字段须填写
// （点选列表项时不要求字段）。返回规范化的动作：label 取自卡片，未声明的字段被拒绝。
func CheckAction(c *v1.Card, a *v1.CardAction) (*v1.CardAction, error) {
	if a.GetCardId() != c.GetCardId() || a.GetCallId() != c.GetCallId() {
		return nil, invalid("action does not belong to this card")
	}
	label, button := "", false
	for _, b := range c.GetButtons() {
		if b.GetId() == a.GetAction() {
			label, button = b.GetLabel(), true
		}
	}
	fields := map[string]*v1.CardField{}
	for _, e := range c.GetBody() {
		if f := e.GetField(); f != nil {
			fields[f.GetId()] = f
		}
		for _, it := range e.GetList().GetItems() {
			if it.GetAction() != "" && it.GetAction() == a.GetAction() {
				label = it.GetTitle()
			}
		}
	}
	if label == "" {
		return nil, invalid("card %s has no action %q", c.GetCardId(), a.GetAction())
	}
	out := &v1.CardAction{CardId: c.GetCardId(), CallId: c.GetCallId(), Action: a.GetAction(), Label: label, Seq: a.GetSeq()}
	for k, v := range a.GetValues() {
		f := fields[k]
		if f == nil {
			return nil, invalid("card %s has no field %q", c.GetCardId(), k)
		}
		if v == "" {
			continue
		}
		if err := checkValue(f, v); err != nil {
			return nil, err
		}
		if out.Values == nil {
			out.Values = map[string]string{}
		}
		out.Values[k] = v
	}
	if button {
		for id, f := range fields {
			if f.GetRequired() && out.GetValues()[id] == "" {
				return nil, invalid("field %s is required", id)
			}
		}
	}
	return out, nil
}

// InputID 是卡片动作的输入 ID：同一版卡片上的同一动作推导出同一 ID，重复点击按已有输入去重。
func InputID(a *v1.CardAction) string {
	return "card_" + shortHash(a.GetCallId()+"\x00"+a.GetAction())
}

// Find 返回结果内容中的卡片。
func Find(content []*v1.ContentBlock) *v1.Card {
	for _, b := range content {
		if c := b.GetCard(); c != nil {
			return c
		}
	}
	return nil
}

// ResultText 是 show_card 结果中给模型看的文字（卡片本身不发给模型：内容是模型自己写的）。
func ResultText(c *v1.Card) string {
	return fmt.Sprintf("已展示卡片 %s（%s）", c.GetCardId(), c.GetTitle())
}

// ActionText 是卡片动作给模型看的文字。
func ActionText(a *v1.CardAction) string { return model.CardActionText(a) }

// Text 是卡片中可见的文字（内容安全检查与搜索用）。
func Text(c *v1.Card) string {
	parts := []string{c.GetTitle(), c.GetSubtitle()}
	for _, e := range c.GetBody() {
		switch {
		case e.GetMarkdown() != nil:
			parts = append(parts, e.GetMarkdown().GetText())
		case e.GetImage() != nil:
			parts = append(parts, e.GetImage().GetAlt())
		case e.GetFacts() != nil:
			for _, f := range e.GetFacts().GetFacts() {
				parts = append(parts, f.GetLabel()+" "+f.GetValue())
			}
		case e.GetList() != nil:
			for _, it := range e.GetList().GetItems() {
				parts = append(parts, it.GetTitle(), it.GetSubtitle())
			}
		case e.GetField() != nil:
			parts = append(parts, e.GetField().GetLabel(), e.GetField().GetValue())
			parts = append(parts, e.GetField().GetOptions()...)
		}
	}
	for _, btn := range c.GetButtons() {
		parts = append(parts, btn.GetLabel())
	}
	return strings.Join(slices.DeleteFunc(parts, func(s string) bool { return s == "" }), "\n")
}

// New 返回 show_card 能力。artifacts 为 nil 时卡片不能引用工件。
func New(artifacts artifact.MetaStore) capability.Capability {
	return capability.Func{
		S: capability.Spec{Name: Capability, Description: Description, InputSchema: json.RawMessage(InputSchema), Idempotent: true},
		Fn: func(ctx context.Context, inv capability.Invocation) ([]*v1.ContentBlock, error) {
			var check ImageCheck
			if artifacts != nil {
				check = func(ctx context.Context, id string) error {
					m, err := artifacts.Get(ctx, id)
					if err != nil {
						return err
					}
					if m.SessionID != inv.SessionID {
						return artifact.ErrNotFound
					}
					return nil
				}
			}
			c, err := Parse(ctx, inv.Arguments, inv.CallID, check)
			if err != nil {
				return nil, err
			}
			return []*v1.ContentBlock{
				{Kind: &v1.ContentBlock_Text{Text: &v1.Text{Text: ResultText(c)}}},
				{Kind: &v1.ContentBlock_Card{Card: c}},
			}, nil
		},
	}
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

func tooLong(s string, n int) bool { return utf8.RuneCountInString(s) > n }

func truncate(s string, n int) string {
	if !tooLong(s, n) {
		return s
	}
	return string([]rune(s)[:n])
}
