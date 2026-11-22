package mcpcap

import (
	"strings"
	"testing"
	"time"
)

func TestParseServer(t *testing.T) {
	t.Setenv("YANSHI_PARSE_TEST_TOKEN", "secret")
	for _, input := range []string{
		"name: xinli\nbusiness_line: eval\nurl: http://localhost/mcp\nheaders:\n  Authorization: ${YANSHI_PARSE_TEST_TOKEN}\nacks: false\ncite_captures: true\ntools:\n  no_approval: [file_changes]\n",
		`{"name":"xinli","business_line":"eval","url":"http://localhost/mcp","headers":{"Authorization":"${YANSHI_PARSE_TEST_TOKEN}"},"acks":true,"cite_captures":true,"tools":{"no_approval":["file_changes"]}}`,
	} {
		got, err := ParseServer([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		if got.Name != "xinli" || got.BusinessLine != "eval" || got.Timeout != 30*time.Second || got.Headers["Authorization"] != "secret" || !got.CiteCaptures || len(got.Tools.NoApproval) != 1 || got.Tools.NoApproval[0] != "file_changes" {
			t.Fatalf("registration: %+v", got)
		}
	}
	for _, input := range []string{
		"[", "name: Bad", "name: x\nbusiness_line: eval\nurl: file:///tmp/x",
		"name: x\nbusiness_line: eval\nurl: http://localhost\ntools:\n  allow: ['[']",
		"name: x\nbusiness_line: eval\nurl: http://localhost\nheaders:\n  Token: ${YANSHI_PARSE_TEST_MISSING}",
	} {
		t.Setenv("YANSHI_PARSE_TEST_MISSING", "")
		if _, err := ParseServer([]byte(input)); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
	got, err := ParseServer([]byte(`{"name":"xinli","business_line":"eval","url":"http://localhost/mcp","timeout":"12s"}`))
	if err != nil || got.Timeout != 12*time.Second {
		t.Fatalf("timeout: %v %v", got.Timeout, err)
	}
}

func TestParseTaintGatedGlobs(t *testing.T) {
	const registration = "name: crm\nbusiness_line: eval\nurl: http://localhost/mcp\ntools:\n  taint_gated: "
	got, err := ParseServer([]byte(registration + "\n    - \"create_*\"\n    - \"update_?\""))
	if err != nil {
		t.Fatal(err)
	}
	if globs := got.Tools.TaintGated; len(globs) != 2 || globs[0] != "create_*" || globs[1] != "update_?" {
		t.Fatalf("taint_gated = %v", globs)
	}
	if _, err := ParseServer([]byte(registration + "\n    - \"[\"")); err == nil || !strings.Contains(err.Error(), "bad tool pattern") {
		t.Fatalf("invalid taint_gated glob: %v", err)
	}
}
