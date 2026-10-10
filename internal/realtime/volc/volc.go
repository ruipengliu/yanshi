// Package volc 是豆包实时语音模型 3.0（Seeduplex，全双工版）的适配器。
//
// 协议：wss://openspeech.bytedance.com/api/v3/duplex/realtime/dialogue，鉴权头 X-Api-Key（新版控制台的 API Key），
// 每个事件是一条 JSON 文本帧，形态接近 OpenAI Realtime。实测（2026-10-11）的行为差异：
//   - 上行音频须严格按实时节奏发送（过快过慢都会报错），由调用方（internal/call）的节拍器保证；
//   - response.output_audio.delta 不带 response_id，按最近一次 output_text.delta / output_audio.started 归属；
//   - 函数调用挂起期间写入的上下文（conversation.item.create）会被忽略；回传结果可以延迟（实测 30 秒仍正常回复）；
//   - 被 response.cancel 中止的回复只有 response.canceled，没有 output_text.done / output_audio.done；
//   - 会话的第一轮约有 5K token 的内置文本输入（之后多数命中缓存）。
package volc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"yanshi/internal/realtime"
)

const DefaultURL = "wss://openspeech.bytedance.com/api/v3/duplex/realtime/dialogue"

// DefaultVoice 是 AgentDef 未指定音色时使用的音色。
const DefaultVoice = "zh_female_vv_jupiter_bigtts"

type Provider struct {
	APIKey string
	// URL 为空时使用 DefaultURL。
	URL string
	// Relaxed 为 true 时使用普通审核。默认严格审核：语音边生成边播放，播出的内容只能依赖厂商的审核
	// （docs/design/m3-call.md §6）。
	Relaxed bool
}

func (p *Provider) url() string {
	if p.URL != "" {
		return p.URL
	}
	return DefaultURL
}

type session struct {
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	events chan realtime.Event

	wmu sync.Mutex
	// response 是正在进行的回复：下行音频不带 response_id，归属于它。
	rmu      sync.Mutex
	response string
	text     strings.Builder
	once     sync.Once
}

// Open 建立连接并创建会话，等到 session.created 后返回。
func (p *Provider) Open(ctx context.Context, cfg realtime.Config) (realtime.Session, error) {
	if p.APIKey == "" {
		return nil, errors.New("volc: API key is not configured")
	}
	h := http.Header{}
	h.Set("X-Api-Key", p.APIKey)
	dialCtx, cancelDial := context.WithTimeout(ctx, 15*time.Second)
	defer cancelDial()
	conn, resp, err := websocket.Dial(dialCtx, p.url(), &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("volc: dial: status %d: %w", resp.StatusCode, err)
		}
		return nil, fmt.Errorf("volc: dial: %w", err)
	}
	conn.SetReadLimit(4 << 20)
	sctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s := &session{conn: conn, ctx: sctx, cancel: cancel, events: make(chan realtime.Event, 256)}

	voice := cfg.Voice
	if voice == "" {
		voice = DefaultVoice
	}
	tools := []map[string]any{}
	for _, t := range cfg.Tools {
		var params any
		if err := json.Unmarshal([]byte(t.ParametersJSON), &params); err != nil {
			conn.CloseNow()
			cancel()
			return nil, fmt.Errorf("volc: tool %s parameters: %w", t.Name, err)
		}
		tools = append(tools, map[string]any{"type": "function", "name": t.Name, "description": t.Description, "parameters": params})
	}
	create := map[string]any{
		"type": "session.create",
		"session": map[string]any{
			"model":        cfg.Model,
			"instructions": cfg.Instructions,
			"audio": map[string]any{
				"input":  map[string]any{"format": map[string]any{"type": "pcm", "rate": realtime.InputSampleRate}},
				"output": map[string]any{"format": map[string]any{"type": "pcm_s16le", "rate": realtime.OutputSampleRate}, "voice": voice},
			},
			"tools": tools,
		},
		"extension": map[string]any{"dialog": map[string]any{"extra": map[string]any{"strict_audit": !p.Relaxed}}},
	}
	if err := s.write(create); err != nil {
		s.Close()
		return nil, err
	}
	// 等待 session.created；之后的事件交给读循环。
	for {
		var ev map[string]any
		if err := s.read(dialCtx, &ev); err != nil {
			s.Close()
			return nil, fmt.Errorf("volc: waiting for session.created: %w", err)
		}
		switch ev["type"] {
		case "session.created":
			go s.loop()
			if len(cfg.History) > 0 {
				if err := s.AddContext(cfg.History); err != nil {
					s.Close()
					return nil, err
				}
			}
			return s, nil
		case "error":
			s.Close()
			return nil, fmt.Errorf("volc: session.create: %s", errorText(ev))
		}
	}
}

func errorText(ev map[string]any) string {
	b, _ := json.Marshal(ev["error"])
	return string(b)
}

func (s *session) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.wmu.Lock()
	defer s.wmu.Unlock()
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	if err := s.conn.Write(ctx, websocket.MessageText, b); err != nil {
		return fmt.Errorf("volc: write: %w", err)
	}
	return nil
}

func (s *session) read(ctx context.Context, v any) error {
	typ, b, err := s.conn.Read(ctx)
	if err != nil {
		return err
	}
	if typ != websocket.MessageText {
		return errors.New("volc: unexpected binary frame")
	}
	return json.Unmarshal(b, v)
}

// wire 是下行事件中用到的字段。
type wire struct {
	Type       string `json:"type"`
	Delta      string `json:"delta"`
	Text       string `json:"text"`
	ResponseID string `json:"response_id"`
	StatusCode string `json:"status_code"`
	Items      []struct {
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"items"`
	Response struct {
		Usage struct {
			InputDetails struct {
				Audio  uint64 `json:"audio_tokens"`
				Text   uint64 `json:"text_tokens"`
				Cached uint64 `json:"cached_tokens"`
			} `json:"input_token_details"`
			OutputDetails struct {
				Audio uint64 `json:"audio_tokens"`
				Text  uint64 `json:"text_tokens"`
			} `json:"output_token_details"`
		} `json:"usage"`
	} `json:"response"`
	Error json.RawMessage `json:"error"`
}

func (s *session) emit(e realtime.Event) {
	select {
	case s.events <- e:
	case <-s.ctx.Done():
	}
}

func (s *session) loop() {
	defer close(s.events)
	for {
		var w wire
		if err := s.read(s.ctx, &w); err != nil {
			if s.ctx.Err() == nil {
				s.emit(realtime.Event{Kind: realtime.Failed, Err: fmt.Errorf("volc: read: %w", err)})
			}
			return
		}
		switch w.Type {
		case "conversation.item.input_audio_transcription.started":
			s.emit(realtime.Event{Kind: realtime.SpeechStarted})
		case "conversation.item.input_audio_transcription.delta":
			// 增量是新识别出的字：累积成识别中的整句。
			s.rmu.Lock()
			s.text.WriteString(w.Delta)
			partial := s.text.String()
			s.rmu.Unlock()
			s.emit(realtime.Event{Kind: realtime.UserTranscript, Text: partial})
		case "conversation.item.input_audio_transcription.completed":
			s.rmu.Lock()
			s.text.Reset()
			s.rmu.Unlock()
			s.emit(realtime.Event{Kind: realtime.UserTranscript, Text: w.Text, Final: true})
		case "response.output_audio.started":
			s.setResponse(w.ResponseID)
		case "response.output_text.delta":
			s.setResponse(w.ResponseID)
			s.emit(realtime.Event{Kind: realtime.AssistantText, ResponseID: w.ResponseID, Text: w.Delta})
		case "response.output_text.done":
			s.emit(realtime.Event{Kind: realtime.AssistantText, ResponseID: w.ResponseID, Text: w.Text, Final: true})
		case "response.output_audio.delta":
			pcm, err := base64.StdEncoding.DecodeString(w.Delta)
			if err != nil {
				s.emit(realtime.Event{Kind: realtime.Failed, Err: fmt.Errorf("volc: audio: %w", err)})
				return
			}
			s.emit(realtime.Event{Kind: realtime.AssistantAudio, ResponseID: s.currentResponse(), Audio: pcm})
		case "response.output_audio.done":
			s.emit(realtime.Event{Kind: realtime.ResponseDone, ResponseID: w.ResponseID})
		case "response.canceled":
			s.emit(realtime.Event{Kind: realtime.ResponseDone, ResponseID: s.currentResponse()})
		case "response.function_call_arguments.done":
			for _, it := range w.Items {
				s.emit(realtime.Event{Kind: realtime.ToolCall, CallID: it.CallID, Name: it.Name, Arguments: it.Arguments})
			}
		case "response.done":
			u := w.Response.Usage
			s.emit(realtime.Event{Kind: realtime.UsageReport, Usage: realtime.Usage{
				InputText: u.InputDetails.Text, InputAudio: u.InputDetails.Audio, CachedInput: u.InputDetails.Cached,
				OutputText: u.OutputDetails.Text, OutputAudio: u.OutputDetails.Audio}})
		case "session.closed":
			return
		case "error":
			s.emit(realtime.Event{Kind: realtime.Failed, Err: fmt.Errorf("volc: %s", string(w.Error))})
			return
		}
	}
}

func (s *session) setResponse(id string) {
	if id == "" {
		return
	}
	s.rmu.Lock()
	s.response = id
	s.rmu.Unlock()
}

func (s *session) currentResponse() string {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	return s.response
}

func (s *session) Audio(pcm []byte) error {
	return s.write(map[string]any{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(pcm)})
}

func (s *session) Mute(muted bool) error {
	if muted {
		return s.write(map[string]any{"type": "input_audio_mute.commit"})
	}
	return s.write(map[string]any{"type": "input_audio_unmute.commit"})
}

func (s *session) CancelResponse() error { return s.write(map[string]any{"type": "response.cancel"}) }

func (s *session) ToolResult(callID, output string) error {
	return s.write(map[string]any{"type": "conversation.item.create", "items": []any{map[string]any{
		"call_id": callID, "role": "tool", "content": []any{map[string]any{"type": "input_text", "text": output}}}}})
}

func (s *session) Say(text string) error {
	return s.write(map[string]any{"type": "speech_text_buffer.commit", "text": text})
}

// maxContextTurns 是一次补充上下文的上限（文档：每次最多 20 轮完整 QA）。
const maxContextTurns = 40

func (s *session) AddContext(turns []realtime.Turn) error {
	if len(turns) > maxContextTurns {
		turns = turns[len(turns)-maxContextTurns:]
	}
	var items []any
	for _, t := range turns {
		// 实测：两种角色的内容都须写成 input_text，写成 output_text 时 assistant 的内容为空。
		items = append(items, map[string]any{"type": "message", "role": t.Role,
			"content": []any{map[string]any{"type": "input_text", "text": t.Text}}})
	}
	if len(items) == 0 {
		return nil
	}
	return s.write(map[string]any{"type": "conversation.item.create", "items": items})
}

func (s *session) Events() <-chan realtime.Event { return s.events }

func (s *session) Close() error {
	s.once.Do(func() {
		_ = s.write(map[string]any{"type": "session.close"})
		s.cancel()
		_ = s.conn.Close(websocket.StatusNormalClosure, "")
	})
	return nil
}
