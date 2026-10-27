package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestParseErrorText(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  string
	}{
		{`[]`, "参数的类型不对"},
		{`{"source":{"day":"yes"}}`, "Source.Day 的类型不对"},
		{`{`, "JSON 格式不正确"},
	} {
		var args changesIn
		err := json.Unmarshal([]byte(tc.input), &args)
		if err == nil {
			t.Fatalf("accepted %s", tc.input)
		}
		got := parseErrorText(err)
		// Go 的字段路径保留字段大小写；对模型只暴露路径，不暴露包名和类型。
		if !strings.EqualFold(got, tc.want) {
			t.Fatalf("parseErrorText = %q, want %q", got, tc.want)
		}
	}
	if got := parseErrorText(errors.New("private implementation detail")); got != "无法解析 JSON 参数" {
		t.Fatal(got)
	}
}
