// Package fake 是脚本化的实时语音模型，用于测试 Call 的中转（internal/call、internal/e2e）。
//
// 它不识别语音：上行的一帧若以 "utt:" 开头，其余部分（去掉末尾的 0 字节）就是用户说的一整句话。
// 对一句话的回应：
//   - "task <内容>"：调用 run_task({"task": 内容})，收到结果后说 "好的。<结果>"；
//   - "long <n>"：说一段 n 个字、n 帧的长回复（每帧间隔 20 ms，文本逐字给出），可被打断；
//   - 其他：说 "你说：<原话>"。
//
// Say 原样说出给定的文字。每次回复都报告用量。
package fake

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"yanshi/internal/realtime"
)

// Utterance 返回一帧"说出 text"的上行音频。
func Utterance(text string) []byte {
	b := make([]byte, realtime.FrameBytes)
	copy(b, "utt:"+text)
	return b
}

type Provider struct {
	mu       sync.Mutex
	sessions []*Session
	// OpenErr 非 nil 时 Open 失败。
	OpenErr error
	// OpenGate 非 nil 时 Open 等它关闭才返回（模拟缓慢的握手），ctx 先结束时返回 ctx 的错误。
	OpenGate chan struct{}
}

// Sessions 返回打开过的会话。
func (p *Provider) Sessions() []*Session {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*Session(nil), p.sessions...)
}

func (p *Provider) Open(octx context.Context, cfg realtime.Config) (realtime.Session, error) {
	if p.OpenErr != nil {
		return nil, p.OpenErr
	}
	if p.OpenGate != nil {
		select {
		case <-p.OpenGate:
		case <-octx.Done():
			return nil, octx.Err()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{Config: cfg, events: make(chan realtime.Event, 1024), ctx: ctx, cancel: cancel}
	s.Context = append(s.Context, cfg.History...)
	p.mu.Lock()
	p.sessions = append(p.sessions, s)
	p.mu.Unlock()
	return s, nil
}

type Session struct {
	Config realtime.Config

	mu sync.Mutex
	// 以下记录收到的调用，供测试检查。
	Frames      int
	Muted       bool
	Said        []string
	Context     []realtime.Turn
	ToolResults map[string]string
	Closed      bool

	events chan realtime.Event
	ctx    context.Context
	cancel context.CancelFunc
	// stop 中止正在进行的回复。
	stop   context.CancelFunc
	nextID int
}

func (s *Session) Audio(pcm []byte) error {
	s.mu.Lock()
	s.Frames++
	closed := s.Closed
	s.mu.Unlock()
	if closed {
		return fmt.Errorf("fake: session closed")
	}
	text, ok := bytes.CutPrefix(pcm, []byte("utt:"))
	if !ok {
		return nil
	}
	utt := string(bytes.TrimRight(text, "\x00"))
	// 用户开口即打断进行中的回复（服务端 VAD）。
	s.interrupt()
	s.emit(realtime.Event{Kind: realtime.SpeechStarted})
	if r := []rune(utt); len(r) > 1 {
		s.emit(realtime.Event{Kind: realtime.UserTranscript, Text: string(r[:len(r)/2])})
	}
	s.emit(realtime.Event{Kind: realtime.UserTranscript, Text: utt, Final: true})
	switch {
	case strings.HasPrefix(utt, "task "):
		s.mu.Lock()
		s.nextID++
		id := fmt.Sprintf("tc%d", s.nextID)
		s.mu.Unlock()
		args, _ := json.Marshal(map[string]string{"task": strings.TrimPrefix(utt, "task ")})
		s.emit(realtime.Event{Kind: realtime.ToolCall, CallID: id, Name: "run_task", Arguments: string(args)})
	case strings.HasPrefix(utt, "long "):
		n, _ := strconv.Atoi(strings.TrimPrefix(utt, "long "))
		s.respond(strings.Repeat("嗯", n), n, 20*time.Millisecond)
	default:
		s.respond("你说："+utt, 3, 0)
	}
	return nil
}

func (s *Session) Mute(muted bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Muted = muted
	return nil
}

func (s *Session) CancelResponse() error {
	s.interrupt()
	return nil
}

func (s *Session) ToolResult(callID, output string) error {
	s.mu.Lock()
	if s.ToolResults == nil {
		s.ToolResults = map[string]string{}
	}
	s.ToolResults[callID] = output
	s.mu.Unlock()
	s.respond("好的。"+output, 3, 0)
	return nil
}

func (s *Session) Say(text string) error {
	s.mu.Lock()
	s.Said = append(s.Said, text)
	s.mu.Unlock()
	s.respond(text, 2, 0)
	return nil
}

func (s *Session) AddContext(turns []realtime.Turn) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Context = append(s.Context, turns...)
	return nil
}

// Snapshot 返回记录的一份副本（加锁读取）。
func (s *Session) Snapshot() (said []string, ctx []realtime.Turn, results map[string]string, frames int, muted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	results = map[string]string{}
	for k, v := range s.ToolResults {
		results[k] = v
	}
	return append([]string(nil), s.Said...), append([]realtime.Turn(nil), s.Context...), results, s.Frames, s.Muted
}

func (s *Session) Events() <-chan realtime.Event { return s.events }

// Fail 模拟服务端出错：发出 Failed 后关闭事件流。
func (s *Session) Fail(err error) {
	s.emit(realtime.Event{Kind: realtime.Failed, Err: err})
	s.Close()
}

func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.Closed {
		s.Closed = true
		if s.stop != nil {
			s.stop()
		}
		s.cancel()
		// 回复的 goroutine 可能仍在发送：留给它们结束后再关闭通道。
		go func() {
			time.Sleep(50 * time.Millisecond)
			close(s.events)
		}()
	}
	return nil
}

func (s *Session) emit(e realtime.Event) {
	select {
	case <-s.ctx.Done():
	case s.events <- e:
	}
}

func (s *Session) interrupt() {
	s.mu.Lock()
	stop := s.stop
	s.stop = nil
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// respond 说一段话。与 Seeduplex 相同，文本先于语音完成：增量、整句、frames 帧音频、结束与用量。
// gap > 0 时（"long"）文本随音频逐字给出、最后才是整句，用来模拟在文本完成之前被打断。被打断时只发出结束。
func (s *Session) respond(text string, frames int, gap time.Duration) {
	s.mu.Lock()
	s.nextID++
	id := fmt.Sprintf("resp%d", s.nextID)
	ctx, stop := context.WithCancel(s.ctx)
	if s.stop != nil {
		s.stop()
	}
	s.stop = stop
	s.mu.Unlock()
	r := []rune(text)
	done := func() {
		s.emit(realtime.Event{Kind: realtime.ResponseDone, ResponseID: id})
		s.emit(realtime.Event{Kind: realtime.UsageReport, Usage: realtime.Usage{InputText: 100, InputAudio: 10, OutputText: uint64(len(r)), OutputAudio: uint64(frames)}})
	}
	if gap == 0 {
		defer stop()
		s.emit(realtime.Event{Kind: realtime.AssistantText, ResponseID: id, Text: string(r[:(len(r)+1)/2])})
		s.emit(realtime.Event{Kind: realtime.AssistantText, ResponseID: id, Text: string(r[(len(r)+1)/2:])})
		s.emit(realtime.Event{Kind: realtime.AssistantText, ResponseID: id, Text: text, Final: true})
		for range frames {
			s.emit(realtime.Event{Kind: realtime.AssistantAudio, ResponseID: id, Audio: make([]byte, 960)})
		}
		done()
		return
	}
	go func() {
		defer stop()
		for i := range frames {
			select {
			case <-ctx.Done():
				s.emit(realtime.Event{Kind: realtime.ResponseDone, ResponseID: id})
				return
			case <-time.After(gap):
			}
			if i < len(r) {
				s.emit(realtime.Event{Kind: realtime.AssistantText, ResponseID: id, Text: string(r[i])})
			}
			s.emit(realtime.Event{Kind: realtime.AssistantAudio, ResponseID: id, Audio: make([]byte, 960)})
		}
		s.emit(realtime.Event{Kind: realtime.AssistantText, ResponseID: id, Text: text, Final: true})
		done()
	}()
}
