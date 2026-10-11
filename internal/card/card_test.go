package card

import (
	"context"
	"errors"
	"strings"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/capability"
	"yanshi/internal/session"
)

func owns(id string) ImageCheck {
	return func(_ context.Context, artifactID string) error {
		if artifactID != id {
			return errors.New("not found")
		}
		return nil
	}
}

const alarm = `{"card_id":"alarm-1","title":"闹钟","image":"icon:alarm","body":[
	{"type":"facts","facts":[{"label":"时间","value":"07:00"},{"label":"设备","value":"手机"}]},
	{"type":"markdown","text":"**明早** 7 点"},
	{"type":"image","uri":"artifact://art_1","alt":"截图"},
	{"type":"list","items":[{"title":"再加一个 7:10","action":"snooze"},{"title":"只是看看"}]},
	{"type":"field","id":"label","label":"备注","input":"text"},
	{"type":"field","id":"repeat","label":"重复","input":"select","options":["不重复","工作日"],"value":"不重复","required":true}],
	"buttons":[{"id":"ok","label":"好的","style":"primary"},{"id":"cancel","label":"取消闹钟","style":"danger"}]}`

func TestParse(t *testing.T) {
	c, err := Parse(context.Background(), alarm, "call_1", owns("art_1"))
	if err != nil {
		t.Fatal(err)
	}
	if c.GetCardId() != "alarm-1" || c.GetCallId() != "call_1" || len(c.GetBody()) != 6 || len(c.GetButtons()) != 2 {
		t.Fatalf("card = %v", c)
	}
	if !strings.Contains(Text(c), "07:00") || !strings.Contains(Text(c), "取消闹钟") {
		t.Fatalf("text = %q", Text(c))
	}
	// 省略 card_id 时按调用生成：同一调用得到同一 ID。
	a, _ := Parse(context.Background(), `{"title":"x"}`, "call_2", nil)
	b, _ := Parse(context.Background(), `{"title":"y"}`, "call_2", nil)
	if a.GetCardId() == "" || a.GetCardId() != b.GetCardId() {
		t.Fatalf("generated ids %q %q", a.GetCardId(), b.GetCardId())
	}
}

func TestParseRejects(t *testing.T) {
	for name, args := range map[string]string{
		"no title":         `{"body":[]}`,
		"external image":   `{"title":"x","image":"https://evil.example/pixel.png"}`,
		"foreign artifact": `{"title":"x","body":[{"type":"image","uri":"artifact://art_other"}]}`,
		"unknown icon":     `{"title":"x","image":"icon:rocket"}`,
		"duplicate action": `{"title":"x","buttons":[{"id":"a","label":"A"},{"id":"a","label":"B"}]}`,
		"bad action id":    `{"title":"x","buttons":[{"id":"a b","label":"A"}]}`,
		"unknown type":     `{"title":"x","body":[{"type":"html","text":"<b>"}]}`,
		"field no button":  `{"title":"x","body":[{"type":"field","id":"f","label":"F"}]}`,
		"bad select value": `{"title":"x","body":[{"type":"field","id":"f","label":"F","input":"select","options":["a"],"value":"b"}],"buttons":[{"id":"ok","label":"OK"}]}`,
		"not json":         `[1]`,
	} {
		if _, err := Parse(context.Background(), args, "call_1", owns("art_1")); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestCheckAction(t *testing.T) {
	c, err := Parse(context.Background(), alarm, "call_1", owns("art_1"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := CheckAction(c, &v1.CardAction{CardId: "alarm-1", CallId: "call_1", Action: "ok", Label: "伪造", Values: map[string]string{"repeat": "工作日", "label": ""}})
	if err != nil || a.GetLabel() != "好的" || a.GetValues()["repeat"] != "工作日" || len(a.GetValues()) != 1 {
		t.Fatalf("action = %v, %v", a, err)
	}
	if !strings.Contains(ActionText(a), "好的") || !strings.Contains(ActionText(a), "repeat=工作日") {
		t.Fatalf("text = %q", ActionText(a))
	}
	// 点选列表项不要求必填字段。
	if a, err := CheckAction(c, &v1.CardAction{CardId: "alarm-1", CallId: "call_1", Action: "snooze"}); err != nil || a.GetLabel() != "再加一个 7:10" {
		t.Fatalf("list action = %v, %v", a, err)
	}
	for name, bad := range map[string]*v1.CardAction{
		"required field":  {CardId: "alarm-1", CallId: "call_1", Action: "ok"},
		"unknown action":  {CardId: "alarm-1", CallId: "call_1", Action: "nope", Values: map[string]string{"repeat": "工作日"}},
		"unknown field":   {CardId: "alarm-1", CallId: "call_1", Action: "cancel", Values: map[string]string{"repeat": "工作日", "x": "1"}},
		"bad option":      {CardId: "alarm-1", CallId: "call_1", Action: "ok", Values: map[string]string{"repeat": "每天"}},
		"other version":   {CardId: "alarm-1", CallId: "call_9", Action: "ok", Values: map[string]string{"repeat": "工作日"}},
		"other card":      {CardId: "alarm-2", CallId: "call_1", Action: "ok", Values: map[string]string{"repeat": "工作日"}},
		"plain list item": {CardId: "alarm-1", CallId: "call_1", Action: ""},
	} {
		if _, err := CheckAction(c, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	// 同一版卡片上的同一动作推导出同一输入 ID；不同版本、不同动作不同。
	id := InputID(&v1.CardAction{CallId: "call_1", Action: "ok"})
	if id != InputID(&v1.CardAction{CallId: "call_1", Action: "ok", Values: map[string]string{"x": "1"}}) ||
		id == InputID(&v1.CardAction{CallId: "call_2", Action: "ok"}) || id == InputID(&v1.CardAction{CallId: "call_1", Action: "cancel"}) {
		t.Fatal("input ids")
	}
}

func TestCapability(t *testing.T) {
	if Capability != session.ShowCardCapability {
		t.Fatalf("session.ShowCardCapability = %q, want %q", session.ShowCardCapability, Capability)
	}
	cp := New(nil)
	out, err := cp.Invoke(context.Background(), capability.Invocation{SessionID: "s", CallID: "call_1", Arguments: `{"title":"天气","image":"icon:weather"}`})
	if err != nil || len(out) != 2 || Find(out).GetTitle() != "天气" || !strings.Contains(out[0].GetText().GetText(), "已展示卡片") {
		t.Fatalf("out = %v, %v", out, err)
	}
	// 没有工件存储时不能引用工件。
	if _, err := cp.Invoke(context.Background(), capability.Invocation{CallID: "c", Arguments: `{"title":"x","image":"artifact://a"}`}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
}
