package volc

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ASRURL 是豆包录音文件识别极速版：一次请求交上整段音频，同步返回文字。按住说话的录音很短，用不着流式识别。
const ASRURL = "https://openspeech.bytedance.com/api/v3/auc/bigmodel/recognize/flash"

// Recognize 识别一段音频（WAV、MP3、OGG 等），返回文字与音频时长。与实时语音、语音合成使用同一个 API Key。
// 没有识别出话语时返回空文字、不报错。
func Recognize(ctx context.Context, apiKey string, audio []byte) (string, time.Duration, error) {
	body, _ := json.Marshal(map[string]any{
		"user":    map[string]any{"uid": "yanshi"},
		"audio":   map[string]any{"data": base64.StdEncoding.EncodeToString(audio)},
		"request": map[string]any{"model_name": "bigmodel", "enable_punc": true},
	})
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ASRURL, bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	var id [16]byte
	_, _ = rand.Read(id[:])
	req.Header.Set("X-Api-Key", apiKey)
	req.Header.Set("X-Api-Resource-Id", "volc.bigasr.auc_turbo")
	req.Header.Set("X-Api-Request-Id", hex.EncodeToString(id[:]))
	req.Header.Set("X-Api-Sequence", "-1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("volc asr: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", 0, fmt.Errorf("volc asr: %w", err)
	}
	// 结果码在响应头里：20000000 成功，20000003 是静音（没有话语）。
	switch code := resp.Header.Get("X-Api-Status-Code"); code {
	case "20000000":
	case "20000003":
		return "", 0, nil
	default:
		return "", 0, fmt.Errorf("volc asr: status %d, code %s %s", resp.StatusCode, code, resp.Header.Get("X-Api-Message"))
	}
	var r struct {
		AudioInfo struct {
			Duration int64 `json:"duration"` // 毫秒
		} `json:"audio_info"`
		Result struct {
			Text string `json:"text"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", 0, fmt.Errorf("volc asr: %w", err)
	}
	return r.Result.Text, time.Duration(r.AudioInfo.Duration) * time.Millisecond, nil
}

// Speech 是豆包语音识别与合成，供按住说话使用（speech.Provider，docs/design/speech.md）。
type Speech struct {
	APIKey string
}

// SpeechRate 是播报的采样率。
const SpeechRate = 24000

func (s *Speech) Recognize(ctx context.Context, audio []byte) (string, time.Duration, error) {
	return Recognize(ctx, s.APIKey, audio)
}

// Synthesize 以默认音色（TTSVoice）返回 PCM 16 位、单声道、SpeechRate 的语音。
func (s *Speech) Synthesize(ctx context.Context, text string) ([]byte, int, error) {
	pcm, err := SynthesizeRate(ctx, s.APIKey, "", text, SpeechRate)
	return pcm, SpeechRate, err
}
