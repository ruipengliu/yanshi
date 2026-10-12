package moderation

import (
	"strings"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/model"
)

// 送检请求只在这里构造：各检查点（提交、回答、Call 转写、Worker）与模拟测试的镜像用同一份规则得到送检文本，
// "检查了什么"不会因调用方不同而分叉。

// ForInput 是用户输入的送检请求：用户的话与卡片动作，界面上下文（它同样由用户提交、会给模型看），
// 以及图片与语音消息工件（模型直接看图、听音频，须检查媒体本身）。
func ForInput(businessLine, sessionID string, input []*v1.ContentBlock) Request {
	req := Request{Stage: Input, BusinessLine: businessLine, SessionID: sessionID, Text: model.Text(input)}
	for _, b := range input {
		if a := b.GetCardAction(); a != nil {
			req.Text += "\n" + model.CardActionText(a)
		}
		if u := b.GetUiContext(); u != nil {
			req.Text += "\n" + strings.Join([]string{u.GetScreen(), u.GetRef(), u.GetSelection(), u.GetContent()}, "\n")
		}
		m := b.GetMedia()
		if m == nil {
			continue
		}
		id, _ := strings.CutPrefix(m.GetUri(), "artifact://")
		switch {
		case strings.HasPrefix(m.GetMimeType(), "image/"):
			req.Images = append(req.Images, Image{ArtifactID: id, MimeType: m.GetMimeType()})
		case strings.HasPrefix(m.GetMimeType(), "audio/"):
			req.Audio = append(req.Audio, Audio{ArtifactID: id, MimeType: m.GetMimeType()})
		}
	}
	return req
}

// ForOutput 是模型输出的送检请求；text 由 runtime.ModerationText 从将要写入日志的消息得到。
func ForOutput(businessLine, sessionID, text string) Request {
	return Request{Stage: Output, BusinessLine: businessLine, SessionID: sessionID, Text: text}
}

// ForTranscript 是 Call 中一句话转写的送检请求：助手的话按输出检查，用户的话按输入检查。
func ForTranscript(businessLine, sessionID, role, text string) Request {
	stage := Input
	if role == "assistant" {
		stage = Output
	}
	return Request{Stage: stage, BusinessLine: businessLine, SessionID: sessionID, Text: text}
}
