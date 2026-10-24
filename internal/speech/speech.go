// Package speech 是按住说话的语音识别与播报（docs/design/speech.md）：录音经识别成为普通的文字输入，Run 的回复
// 经合成读给用户。模型只看到文字，音频既不交给模型，也不进日志或工件：识别与合成是无状态的转换，结果只返回给
// 发起的客户端。识别出的文字由客户端作为输入提交，内容安全检查与键入的文字相同；播报只读 Run 已写入日志的回复，
// 那是检查过的输出。
package speech

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"yanshi/internal/ids"
	"yanshi/internal/model"
	"yanshi/internal/service"
	"yanshi/internal/session"
	"yanshi/internal/usage"
)

// Provider 是语音识别与合成的提供商。
type Provider interface {
	// Recognize 识别一段音频，返回文字与音频时长；没有话语时返回空文字。
	Recognize(ctx context.Context, audio []byte) (string, time.Duration, error)
	// Synthesize 把 text 合成为 PCM 16 位单声道语音，返回采样率。
	Synthesize(ctx context.Context, text string) ([]byte, int, error)
}

var (
	// ErrNotConfigured 表示部署没有配置语音提供商。
	ErrNotConfigured = errors.New("speech is not configured")
	// ErrProvider 表示语音提供商调用失败。
	ErrProvider = errors.New("speech provider unavailable")
)

const (
	// MaxAudioBytes 是一段录音的上限：16 kHz WAV 约两分钟，按住说话用不着更长。
	MaxAudioBytes = 4 << 20
	// MaxSpeakRunes 是播报的字数上限：更长的回复听起来负担太重，读到上限之前最后一个句末为止。
	MaxSpeakRunes = 600
)

type Service struct {
	Sessions *service.Service
	// Provider 为 nil 时两个接口都返回 ErrNotConfigured。
	Provider Provider
	// ASRModel、TTSModel 是计量与价格表中的模型引用（如 "volc/asr-flash"）。
	ASRModel, TTSModel string
	Meter              *usage.Meter
	IDs                ids.Generator
	Logger             *slog.Logger
}

func (s *Service) log() *slog.Logger {
	if s.Logger == nil {
		return slog.Default()
	}
	return s.Logger
}

// Transcribe 识别一段录音。调用方须已确认请求方有权访问 st 所属的 Session。与 Call 一样，配额用尽时拒绝。
func (s *Service) Transcribe(ctx context.Context, st *session.State, audio []byte) (string, error) {
	if s == nil || s.Provider == nil {
		return "", ErrNotConfigured
	}
	if len(audio) == 0 || len(audio) > MaxAudioBytes {
		return "", fmt.Errorf("%w: audio must be 1 to %d bytes", service.ErrInvalid, MaxAudioBytes)
	}
	if err := s.Sessions.CheckQuota(ctx, st, "speech"); err != nil {
		return "", err
	}
	text, d, err := s.Provider.Recognize(ctx, audio)
	if err != nil {
		s.log().Warn("speech recognize failed", "session", st.SessionID, "err", err)
		return "", fmt.Errorf("%w: %v", ErrProvider, err)
	}
	s.meter(ctx, st, s.ASRModel, uint64((d+time.Second-1)/time.Second), 0)
	return strings.TrimSpace(text), nil
}

// Speak 把 Run 的回复合成为 WAV。Run 不在投影中时返回 ErrNotFound，还没有文字回复时返回 ErrConflict。
// 音色是提供商的默认音色：AgentDef 的通话音色属于实时语音模型，语音合成不一定有。
func (s *Service) Speak(ctx context.Context, st *session.State, runID string) ([]byte, error) {
	if s == nil || s.Provider == nil {
		return nil, ErrNotConfigured
	}
	text, ok := Reply(st, runID)
	if !ok {
		return nil, fmt.Errorf("%w: run %s", service.ErrNotFound, runID)
	}
	text = Speakable(text)
	if text == "" {
		return nil, fmt.Errorf("%w: run %s has no reply to speak", service.ErrConflict, runID)
	}
	if err := s.Sessions.CheckQuota(ctx, st, "speech"); err != nil {
		return nil, err
	}
	pcm, rate, err := s.Provider.Synthesize(ctx, text)
	if err != nil {
		s.log().Warn("speech synthesize failed", "session", st.SessionID, "run", runID, "err", err)
		return nil, fmt.Errorf("%w: %v", ErrProvider, err)
	}
	s.meter(ctx, st, s.TTSModel, 0, uint64(utf8.RuneCountInString(text)))
	return WAV(pcm, rate), nil
}

func (s *Service) meter(ctx context.Context, st *session.State, model string, input, output uint64) {
	if s.Meter == nil {
		return
	}
	id := "speech_" + s.IDs()
	agent := st.Agent.GetName() + "@" + st.Agent.GetVersion()
	s.Meter.Speech(ctx, usage.Scope{SessionID: st.SessionID, BusinessLine: st.Created.GetBusinessLine(), EndUser: st.Created.GetEndUser()},
		id, agent, model, input, output, time.Now())
}

// Reply 返回 Run 最后一条有文字的模型输出；ok 为 false 表示 Run 不在投影中。
// 上下文压缩之后，压缩前的输出不再在 History 中，此时返回空文字。
func Reply(st *session.State, runID string) (string, bool) {
	found := false
	for _, r := range st.Runs {
		if r.ID == runID {
			found = true
		}
	}
	if !found {
		return "", false
	}
	for i := len(st.History) - 1; i >= 0; i-- {
		m := st.History[i].GetAssistantMessage()
		if m.GetRunId() != runID {
			continue
		}
		if t := strings.TrimSpace(model.Text(m.GetContent())); t != "" {
			return t, true
		}
	}
	return "", true
}

var (
	mdLink   = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	mdMarker = regexp.MustCompile(`(?m)^[ \t]*(#{1,6}[ \t]+|[-*+][ \t]+|>[ \t]*|\d+\.[ \t]+)`)
	mdInline = strings.NewReplacer("**", "", "__", "", "`", "", "|", "，", "~~", "")
)

// Speakable 把 Markdown 回复改写为适合朗读的文字：去掉标题、列表与强调的记号，链接只读文字，再截到 MaxSpeakRunes。
func Speakable(text string) string {
	text = mdLink.ReplaceAllString(text, "$1")
	text = mdMarker.ReplaceAllString(text, "")
	text = strings.TrimSpace(mdInline.Replace(text))
	if utf8.RuneCountInString(text) <= MaxSpeakRunes {
		return text
	}
	r := []rune(text)[:MaxSpeakRunes]
	for i := len(r) - 1; i > MaxSpeakRunes/2; i-- {
		if strings.ContainsRune("。！？!?\n", r[i]) {
			return string(r[:i+1])
		}
	}
	return string(r)
}

// WAV 给 PCM 16 位单声道加上 WAV 头，浏览器可以直接播放。
func WAV(pcm []byte, rate int) []byte {
	b := make([]byte, 44, 44+len(pcm))
	copy(b[0:], "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+len(pcm)))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1) // PCM
	binary.LittleEndian.PutUint16(b[22:], 1) // 单声道
	binary.LittleEndian.PutUint32(b[24:], uint32(rate))
	binary.LittleEndian.PutUint32(b[28:], uint32(rate*2))
	binary.LittleEndian.PutUint16(b[32:], 2)
	binary.LittleEndian.PutUint16(b[34:], 16)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(len(pcm)))
	return append(b, pcm...)
}
