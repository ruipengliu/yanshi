package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestParseErrorText(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  string
	}{
		{"root array", `[]`, "某个参数应为对象，收到的是数组"},
		{"object field", `{"source":{"day":"yes"}}`, "Source.Day 应为布尔值，收到的是字符串"},
		{"source string", `{"source":"#57.0–#80.2"}`, "Source 应为对象，收到的是字符串"},
		{"number", `{"source":{"from":1.5}}`, "Source.From 应为字符串，收到的是数字"},
		{"syntax", `{`, "JSON 格式不正确"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var args changesIn
			err := json.Unmarshal([]byte(tc.input), &args)
			if err == nil {
				t.Fatalf("accepted %s", tc.input)
			}
			if got := parseErrorText(err); !strings.EqualFold(got, tc.want) {
				t.Fatalf("parseErrorText = %q, want %q", got, tc.want)
			}
			c := &Call{args: json.RawMessage(tc.input)}
			if got := c.FileChanges(); !strings.EqualFold(got, "参数无效："+tc.want) {
				t.Fatalf("FileChanges = %q, want %q", got, "参数无效："+tc.want)
			}
		})
	}
	if got := parseErrorText(errors.New("private implementation detail")); got != "无法解析 JSON 参数" {
		t.Fatal(got)
	}
}

func TestParseErrorTextStringArrayElements(t *testing.T) {
	// 这两个字段允许单个字符串，但数组里的每项仍须是字符串，不能接收模型给的对象。
	for _, field := range []string{"risks", "awareness"} {
		t.Run(field, func(t *testing.T) {
			input := fmt.Sprintf(`{"source":{"day":true},"exchanges":{"create":[{"%s":[{"text":"内容"}]}]}}`, field)
			var args changesIn
			err := json.Unmarshal([]byte(input), &args)
			var mismatch *json.UnmarshalTypeError
			if !errors.As(err, &mismatch) {
				t.Fatalf("expected type error, got %v", err)
			}
			// 自定义解码会丢失祖先路径；保留解码器实际给出的路径，不虚构字段或索引。
			path := strings.TrimPrefix(mismatch.Field, ".")
			if path == "" {
				path = "某个参数"
			} else {
				path += " "
			}
			want := "参数无效：" + path + "应为字符串，收到的是对象"
			c := &Call{args: json.RawMessage(input)}
			if got := c.FileChanges(); got != want {
				t.Fatalf("FileChanges = %q, want %q", got, want)
			}
		})
	}
}

func TestParseErrorTextPaths(t *testing.T) {
	for _, tc := range []struct {
		field string
		want  string
	}{
		{"exchanges.create.0.risks.0", "exchanges.create.0.risks.0 应为字符串，收到的是对象"},
		{".0", "0 应为字符串，收到的是对象"},
		{"0", "0 应为字符串，收到的是对象"},
		{"", "某个参数应为字符串，收到的是对象"},
	} {
		err := &json.UnmarshalTypeError{Field: tc.field, Type: reflect.TypeOf(""), Value: "object", Struct: "privateType"}
		if got := parseErrorText(fmt.Errorf("private package: %w", err)); got != tc.want {
			t.Fatalf("parseErrorText(%q) = %q, want %q", tc.field, got, tc.want)
		}
	}
}

func TestParseErrorTextTypes(t *testing.T) {
	type privateString string
	for _, tc := range []struct {
		typ  reflect.Type
		want string
	}{
		{reflect.TypeOf(privateString("")), "字符串"},
		{reflect.TypeOf((**privateString)(nil)), "字符串"},
		{reflect.TypeOf(int(0)), "数字"},
		{reflect.TypeOf(int8(0)), "数字"},
		{reflect.TypeOf(int16(0)), "数字"},
		{reflect.TypeOf(int32(0)), "数字"},
		{reflect.TypeOf(int64(0)), "数字"},
		{reflect.TypeOf(uint(0)), "数字"},
		{reflect.TypeOf(uint8(0)), "数字"},
		{reflect.TypeOf(uint16(0)), "数字"},
		{reflect.TypeOf(uint32(0)), "数字"},
		{reflect.TypeOf(uint64(0)), "数字"},
		{reflect.TypeOf(uintptr(0)), "数字"},
		{reflect.TypeOf(float32(0)), "数字"},
		{reflect.TypeOf(float64(0)), "数字"},
		{reflect.TypeOf(false), "布尔值"},
		{reflect.TypeOf([]string{}), "数组"},
		{reflect.TypeOf([1]string{}), "数组"},
		{reflect.TypeOf(map[string]string{}), "对象"},
		{reflect.TypeOf(changesIn{}), "对象"},
		{reflect.TypeOf((*any)(nil)), "另一种类型"},
		{reflect.TypeOf(complex64(0)), "另一种类型"},
		{nil, "另一种类型"},
	} {
		err := &json.UnmarshalTypeError{Type: tc.typ, Value: "string"}
		want := "某个参数应为" + tc.want + "，收到的是字符串"
		if got := parseErrorText(err); got != want {
			t.Fatalf("parseErrorText(%v) = %q, want %q", tc.typ, got, want)
		}
	}
	for _, tc := range []struct {
		value string
		want  string
	}{
		{"object", "对象"},
		{"array", "数组"},
		{"string", "字符串"},
		{"bool", "布尔值"},
		{"number", "数字"},
		{"number 1.5", "数字"},
		{"number -2", "数字"},
		{"null", "另一种类型"},
		{"private implementation detail", "另一种类型"},
	} {
		err := &json.UnmarshalTypeError{Type: reflect.TypeOf(""), Value: tc.value}
		want := "某个参数应为字符串，收到的是" + tc.want
		if got := parseErrorText(err); got != want {
			t.Fatalf("parseErrorText(%q) = %q, want %q", tc.value, got, want)
		}
	}
}
