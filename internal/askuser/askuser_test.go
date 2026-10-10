package askuser

import (
	"errors"
	"strings"
	"testing"

	"yanshi/internal/model"
)

const rooms = `{"question":"订哪个会议室？","options":[{"id":"a","label":"A201（10 人）"},{"id":"b","label":"B302（6 人）"}],
"fields":[{"name":"date","label":"日期","type":"date","required":true},{"name":"people","label":"人数","type":"number"}]}`

func TestParse(t *testing.T) {
	q, err := Parse(rooms)
	if err != nil {
		t.Fatal(err)
	}
	if len(q.Options) != 2 || q.Fields[0].Type != "date" {
		t.Fatalf("parsed %+v", q)
	}
	if q, _ := Parse(`{"question":"q","fields":[{"name":"x","label":"X"}]}`); q.Fields[0].Type != "text" {
		t.Fatal("field type should default to text")
	}
	for _, bad := range []string{
		`not json`,
		`{"question":""}`,
		`{"question":"q","options":[{"id":"a","label":"only one"}]}`,
		`{"question":"q","options":[{"id":"a","label":"x"},{"id":"a","label":"y"}]}`,
		`{"question":"q","options":[{"id":"a","label":""},{"id":"b","label":"y"}]}`,
		`{"question":"q","fields":[{"name":"x","label":"X","type":"color"}]}`,
		`{"question":"` + strings.Repeat("长", 501) + `"}`,
	} {
		if _, err := Parse(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("Parse(%.40s) = %v, want ErrInvalid", bad, err)
		}
	}
}

func TestCheck(t *testing.T) {
	q, _ := Parse(rooms)
	ok := []*Answer{
		{Selected: []string{"b"}, Values: map[string]string{"date": "2026-10-12"}},
		{Selected: []string{"a"}, Values: map[string]string{"date": "2026-10-12", "people": "5"}},
		{Text: "都不合适，换一天吧"}, // 直接输入文字总是可以
	}
	for _, a := range ok {
		if err := q.Check(a); err != nil {
			t.Errorf("Check(%+v) = %v", a, err)
		}
	}
	bad := []*Answer{
		{},
		{Selected: []string{"a", "b"}, Values: map[string]string{"date": "2026-10-12"}}, // 单选
		{Selected: []string{"c"}, Values: map[string]string{"date": "2026-10-12"}},      // 未知选项
		{Selected: []string{"a"}}, // 缺必填字段
		{Selected: []string{"a"}, Values: map[string]string{"date": "明天"}},
		{Selected: []string{"a"}, Values: map[string]string{"date": "2026-10-12", "people": "五"}},
		{Selected: []string{"a"}, Values: map[string]string{"date": "2026-10-12", "floor": "3"}},
	}
	for _, a := range bad {
		if err := q.Check(a); !errors.Is(err, ErrInvalid) {
			t.Errorf("Check(%+v) = %v, want ErrInvalid", a, err)
		}
	}
}

func TestResultCarriesLabels(t *testing.T) {
	q, _ := Parse(rooms)
	got := model.Text(Result(q, &Answer{Selected: []string{"b"}, Values: map[string]string{"date": "2026-10-12", "people": ""}}))
	want := `{"selected":[{"id":"b","label":"B302（6 人）"}],"values":{"date":"2026-10-12"}}`
	if got != want {
		t.Fatalf("result = %s, want %s", got, want)
	}
}
