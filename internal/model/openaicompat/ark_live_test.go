package openaicompat

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"yanshi/internal/model"
)

// TestArkLive 用真实请求确认火山方舟的接入：思考强度后缀、图片输入、多模态嵌入（YANSHI_TEST_ARK=1 且设置 ARK_API_KEY）。
func TestArkLive(t *testing.T) {
	key := os.Getenv("ARK_API_KEY")
	if os.Getenv("YANSHI_TEST_ARK") == "" || key == "" {
		t.Skip("YANSHI_TEST_ARK and ARK_API_KEY not set")
	}
	base := os.Getenv("ARK_BASE_URL")
	if base == "" {
		base = ArkBaseURL
	}
	p := &Provider{BaseURL: base, APIKey: key}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	resp, err := p.Generate(ctx, &model.Request{Model: "doubao-seed-2-1-lite-260915:minimal", Messages: []model.Message{
		{Role: model.RoleUser, Content: model.TextBlocks("只回答数字：1+1=?")},
	}}, nil)
	if err != nil || !strings.Contains(model.Text(resp.Content), "2") {
		t.Fatalf("generate: %v %v", resp, err)
	}
	vecs, err := p.Embed(ctx, "doubao-embedding-vision-251215", []string{"闹钟", "天气"})
	if err != nil || len(vecs) != 2 || len(vecs[0]) == 0 {
		t.Fatalf("embed: %v", err)
	}
}
