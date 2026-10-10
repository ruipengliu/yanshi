package volc

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"yanshi/internal/realtime"
)

// TestLive 对真实服务走一遍：识别 → 函数调用 → 回传结果 → 语音回复 → 用量。消耗少量 token，
// 需要 YANSHI_TEST_VOLC 与 VOLC_SPEECH_API_KEY，不在 make check 中。
// testdata/weather.pcm 是 macOS say 合成的"帮我查一下上海明天的天气。"（16 kHz PCM）。
func TestLive(t *testing.T) {
	key := os.Getenv("VOLC_SPEECH_API_KEY")
	if os.Getenv("YANSHI_TEST_VOLC") == "" || key == "" {
		t.Skip("YANSHI_TEST_VOLC and VOLC_SPEECH_API_KEY not set")
	}
	pcm, err := os.ReadFile("testdata/weather.pcm")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s, err := (&Provider{APIKey: key}).Open(ctx, realtime.Config{Model: "1.2.6.1", Instructions: "你是简洁的中文语音助手。查询天气时调用工具。",
		Tools: []realtime.Tool{{Name: "get_weather", Description: "查询某城市某天的天气",
			ParametersJSON: `{"type":"object","properties":{"city":{"type":"string"},"date":{"type":"string"}},"required":["city"]}`}},
		History: []realtime.Turn{{Role: "user", Text: "我住在上海。"}, {Role: "assistant", Text: "好的，记住了。"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	go func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		silence := make([]byte, realtime.FrameBytes)
		for i := 0; ctx.Err() == nil; i += realtime.FrameBytes {
			<-tick.C
			frame := silence
			if i+realtime.FrameBytes <= len(pcm) {
				frame = pcm[i : i+realtime.FrameBytes]
			}
			if s.Audio(frame) != nil {
				return
			}
		}
	}()
	var transcript, reply string
	var audio int
	var usage realtime.Usage
	called := false
	for ev := range s.Events() {
		switch ev.Kind {
		case realtime.UserTranscript:
			if ev.Final {
				transcript = ev.Text
			}
		case realtime.ToolCall:
			if ev.Name != "get_weather" || !strings.Contains(ev.Arguments, "上海") {
				t.Fatalf("tool call %s(%s)", ev.Name, ev.Arguments)
			}
			called = true
			if err := s.ToolResult(ev.CallID, `{"weather":"小雨转阴","temp":"17~22℃"}`); err != nil {
				t.Fatal(err)
			}
		case realtime.AssistantText:
			if ev.Final {
				reply = ev.Text
			}
		case realtime.AssistantAudio:
			audio += len(ev.Audio)
		case realtime.UsageReport:
			usage.Add(ev.Usage)
		case realtime.Failed:
			t.Fatal(ev.Err)
		}
		if called && reply != "" && usage.OutputAudio > 0 {
			break
		}
	}
	t.Logf("transcript %q, reply %q, audio %.1fs, usage %+v", transcript, reply, float64(audio)/(2*realtime.OutputSampleRate), usage)
	if !strings.Contains(transcript, "天气") || !called || !strings.Contains(reply, "雨") || audio == 0 {
		t.Fatal("incomplete round trip")
	}
}

// TestSynthesize 合成一句话（需要 YANSHI_TEST_VOLC）。
func TestSynthesize(t *testing.T) {
	key := os.Getenv("VOLC_SPEECH_API_KEY")
	if os.Getenv("YANSHI_TEST_VOLC") == "" || key == "" {
		t.Skip("YANSHI_TEST_VOLC and VOLC_SPEECH_API_KEY not set")
	}
	pcm, err := Synthesize(context.Background(), key, "", "你好，现在几点了？")
	if err != nil {
		t.Fatal(err)
	}
	if secs := float64(len(pcm)) / 32000; secs < 0.8 || secs > 5 {
		t.Fatalf("synthesized %.1fs of audio", secs)
	}
}
