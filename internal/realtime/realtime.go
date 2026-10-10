// Package realtime 是与厂商无关的实时语音模型接口（docs/design/m3-call.md §3）：Call 经它与端到端语音模型
// 双向流式交互。适配器按厂商实现（volc：豆包实时语音 Seeduplex），与 model.Provider 的模型中立思路一致（G9）。
//
// 音频格式固定：上行 PCM 16 位小端、单声道、16 kHz，每帧 20 ms；下行 PCM 16 位小端、单声道、24 kHz。
// 轮次判断（VAD）与打断由模型在服务端完成。
package realtime

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

const (
	InputSampleRate  = 16000
	OutputSampleRate = 24000
	// FrameBytes 是上行一帧（20 ms）的字节数。
	FrameBytes = InputSampleRate / 50 * 2
)

// Tool 是提供给语音模型的函数（JSON Schema 参数）。
type Tool struct {
	Name           string
	Description    string
	ParametersJSON string
}

// Turn 是一句对话，用于初始化或补充模型的上下文。
type Turn struct {
	// Role 是 "user" 或 "assistant"。
	Role string
	Text string
}

type Config struct {
	// Model 是厂商内的模型名（引用 "provider/model" 中 "/" 之后的部分）。
	Model        string
	Instructions string
	Voice        string
	Tools        []Tool
	// History 是开始时放入模型上下文的近期对话（旧的在前）。
	History []Turn
}

// Session 是与模型的一次实时会话。方法可并发调用。
type Session interface {
	// Audio 发送一帧上行音频（FrameBytes 字节）。调用方负责按实时节奏发送。
	Audio(pcm []byte) error
	// Mute 告诉模型麦克风已关闭 / 打开：关闭期间不发送音频，模型不会因收不到音频而超时。
	Mute(muted bool) error
	// CancelResponse 中止正在进行的回复（用户在界面上打断）。
	CancelResponse() error
	// ToolResult 回传函数调用的结果，模型据此继续回复。
	ToolResult(callID, output string) error
	// Say 让模型原样说出 text（如后台任务完成后的播报）。
	Say(text string) error
	// AddContext 把对话补充进模型上下文，不触发回复。
	AddContext(turns []Turn) error
	// Events 返回事件流；会话结束时关闭。
	Events() <-chan Event
	// Close 结束会话。
	Close() error
}

type Provider interface {
	Open(ctx context.Context, cfg Config) (Session, error)
}

type EventKind int

const (
	// SpeechStarted：检测到用户开始说话，客户端应停止播放（打断）。
	SpeechStarted EventKind = iota + 1
	// UserTranscript：用户的话；Final 为 false 时是识别中的整句（替换上一条）。
	UserTranscript
	// AssistantText：助手回复的文本；Final 为 false 时 Text 是增量，为 true 时是整句。
	AssistantText
	// AssistantAudio：助手语音（Audio）。
	AssistantAudio
	// ResponseDone：一次回复的语音结束。
	ResponseDone
	// ToolCall：模型调用函数（CallID、Name、Arguments）。
	ToolCall
	// UsageReport：一轮交互的用量。
	UsageReport
	// Failed：会话出错（Err），随后事件流关闭。
	Failed
)

type Event struct {
	Kind       EventKind
	ResponseID string
	Text       string
	Final      bool
	Audio      []byte
	CallID     string
	Name       string
	Arguments  string
	Usage      Usage
	Err        error
}

// Usage 是一轮交互的 token 用量。CachedInput 是输入中命中缓存的部分（文本与音频合计）。
type Usage struct {
	InputText, InputAudio, CachedInput, OutputText, OutputAudio uint64
}

func (u *Usage) Add(o Usage) {
	u.InputText += o.InputText
	u.InputAudio += o.InputAudio
	u.CachedInput += o.CachedInput
	u.OutputText += o.OutputText
	u.OutputAudio += o.OutputAudio
}

// Gateway 按 "provider/model" 引用选择 Provider。
type Gateway struct {
	mu        sync.RWMutex
	providers map[string]Provider
}

func NewGateway() *Gateway { return &Gateway{providers: map[string]Provider{}} }

func (g *Gateway) Register(name string, p Provider) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.providers[name] = p
}

// Has 报告 ref 的 provider 是否已注册。
func (g *Gateway) Has(ref string) bool {
	name, _, ok := strings.Cut(ref, "/")
	g.mu.RLock()
	defer g.mu.RUnlock()
	return ok && g.providers[name] != nil
}

// Open 以 ref（"provider/model"）打开会话；cfg.Model 由 ref 填写。
func (g *Gateway) Open(ctx context.Context, ref string, cfg Config) (Session, error) {
	name, m, ok := strings.Cut(ref, "/")
	g.mu.RLock()
	p := g.providers[name]
	g.mu.RUnlock()
	if !ok || p == nil {
		return nil, fmt.Errorf("realtime: unknown provider in %q", ref)
	}
	cfg.Model = m
	return p.Open(ctx, cfg)
}
