package moderation_test

import (
	"reflect"
	"testing"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/model"
	"yanshi/internal/moderation"
)

func TestForInput(t *testing.T) {
	action := &v1.CardAction{CardId: "card1", Action: "ok", Label: "确定"}
	input := append(model.TextBlocks("看看这个"),
		&v1.ContentBlock{Kind: &v1.ContentBlock_CardAction{CardAction: action}},
		&v1.ContentBlock{Kind: &v1.ContentBlock_UiContext{UiContext: &v1.UIContext{Screen: "订单详情", Ref: "order://A1", Selection: "选中", Content: "可见", Device: "phone"}}},
		&v1.ContentBlock{Kind: &v1.ContentBlock_Media{Media: &v1.Media{MimeType: "image/png", Uri: "artifact://art_img"}}},
		&v1.ContentBlock{Kind: &v1.ContentBlock_Media{Media: &v1.Media{MimeType: "audio/wav", Uri: "artifact://art_voice"}}},
		&v1.ContentBlock{Kind: &v1.ContentBlock_Media{Media: &v1.Media{MimeType: "application/pdf", Uri: "artifact://art_doc"}}},
	)
	got := moderation.ForInput("bl", "s1", input)
	want := moderation.Request{
		Stage: moderation.Input, BusinessLine: "bl", SessionID: "s1",
		// 设备标签由平台填写，不是用户提交的内容，不送检。
		Text:   "看看这个\n" + model.CardActionText(action) + "\n订单详情\norder://A1\n选中\n可见",
		Images: []moderation.Image{{ArtifactID: "art_img", MimeType: "image/png"}},
		Audio:  []moderation.Audio{{ArtifactID: "art_voice", MimeType: "audio/wav"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ForInput:\n got %+v\nwant %+v", got, want)
	}
}

func TestForOutputAndTranscript(t *testing.T) {
	if got := moderation.ForOutput("bl", "s1", "好的"); !reflect.DeepEqual(got, moderation.Request{Stage: moderation.Output, BusinessLine: "bl", SessionID: "s1", Text: "好的"}) {
		t.Fatalf("ForOutput: %+v", got)
	}
	for role, stage := range map[string]moderation.Stage{"assistant": moderation.Output, "user": moderation.Input} {
		got := moderation.ForTranscript("bl", "s1", role, "你好")
		if got.Stage != stage || got.BusinessLine != "bl" || got.SessionID != "s1" || got.Text != "你好" {
			t.Fatalf("ForTranscript(%s): %+v", role, got)
		}
	}
}
