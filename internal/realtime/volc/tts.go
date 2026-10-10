package volc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// TTSURL 是豆包语音合成的 HTTP 单向流式接口。
const TTSURL = "https://openspeech.bytedance.com/api/v3/tts/unidirectional"

// TTSVoice 是评测合成用户语音的默认音色（豆包语音合成模型 2.0）。
const TTSVoice = "zh_female_vv_uranus_bigtts"

// Synthesize 把 text 合成为 PCM 16 位、单声道、16 kHz 的语音。用于评测：以文字写用例，合成为"用户说的话"
// 送入 Call（docs/design/m3-call.md §9）。与实时语音使用同一个 API Key。
func Synthesize(ctx context.Context, apiKey, voice, text string) ([]byte, error) {
	if voice == "" {
		voice = TTSVoice
	}
	body, _ := json.Marshal(map[string]any{
		"user": map[string]any{"uid": "yanshi-eval"},
		"req_params": map[string]any{"text": text, "speaker": voice,
			"audio_params": map[string]any{"format": "pcm", "sample_rate": 16000}},
	})
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TTSURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", apiKey)
	req.Header.Set("X-Api-Resource-Id", "seed-tts-2.0")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("volc tts: %w", err)
	}
	defer resp.Body.Close()
	// 响应是逐行的 JSON：音频块（data 为 base64）、句子信息，最后是 code 20000000 的结束包。
	var pcm []byte
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var m struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    string `json:"data"`
			Header  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"header"`
		}
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			return nil, fmt.Errorf("volc tts: %w", err)
		}
		switch {
		case m.Header != nil && m.Header.Code != 0:
			return nil, fmt.Errorf("volc tts: %d %s", m.Header.Code, m.Header.Message)
		case m.Code == 20000000:
			return pcm, nil
		case m.Code != 0:
			return nil, fmt.Errorf("volc tts: %d %s", m.Code, m.Message)
		case m.Data != "":
			b, err := base64.StdEncoding.DecodeString(m.Data)
			if err != nil {
				return nil, fmt.Errorf("volc tts: %w", err)
			}
			pcm = append(pcm, b...)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("volc tts: %w", err)
	}
	if len(pcm) == 0 {
		return nil, fmt.Errorf("volc tts: no audio (status %d)", resp.StatusCode)
	}
	return pcm, nil
}
