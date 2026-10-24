package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/model"
	"yanshi/internal/moderation"
	"yanshi/internal/session"
)

// CaptureResult 是提交旁听转写的结果。
type CaptureResult struct {
	// Duplicate 为 true 表示同一 capture_id 此前已写入，本次没有写入。
	Duplicate bool
	// Redacted 是本批中未通过内容安全、只留占位的段数；重复提交时为 0。
	Redacted int
}

// SubmitCaptures 写入一批旁听转写（docs/design/captures.md §3，ADR-0036）。它不开启 Run、不唤醒 Session，因此不做准入与
// 配额检查。captureID 由 HostApp 生成：结果未知时以同一 ID 重新提交是安全的，去重窗口是最近 session.MaxRecentCaptures 批。
// 只有开启了旁听的 AgentDef（agentdef.Def.Captures）的 Session 接收。
func (s *Service) SubmitCaptures(ctx context.Context, sessionID, captureID string, segments []*v1.CaptureSegment) (*CaptureResult, error) {
	if len(captureID) > MaxInputIDLen {
		return nil, fmt.Errorf("%w: capture_id exceeds %d bytes", ErrInvalid, MaxInputIDLen)
	}
	for _, g := range segments {
		// 拦截只由平台判断：客户端自报的占位会让一段话无从检查地消失，也会被当作内容安全的结果展示。
		if g.GetRedacted() {
			return nil, fmt.Errorf("%w: redacted is set by the platform", ErrInvalid)
		}
		// 时区写错时整理会显示错误的日期与时刻：拒绝，而不是悄悄不显示。
		if z := g.GetTimeZone(); z != "" {
			if _, ok := model.CaptureZone(z); !ok {
				return nil, fmt.Errorf("%w: time_zone %q is not an IANA time zone", ErrInvalid, z)
			}
		}
	}
	m := &v1.CaptureAppended{CaptureId: captureID, Segments: segments}
	if err := session.CheckCapture(m); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	st, err := s.Load(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	// 重复提交在内容安全之前返回：首次已经检查过。
	if st.Captured(captureID) {
		return &CaptureResult{Duplicate: true}, nil
	}
	def, err := s.Agents.Get(st.Agent)
	if err != nil {
		return nil, err
	}
	if !def.Captures {
		return nil, fmt.Errorf("%w: agent %s does not accept captures", ErrInvalid, def.Name)
	}
	res := &CaptureResult{}
	if m.Segments, res.Redacted, err = s.moderateCaptures(ctx, st, segments); err != nil {
		return nil, err
	}
	var prev time.Time
	err = s.Store.Update(ctx, st, func(st *session.State) (session.Decision, error) {
		// 并发的同 ID 提交可能刚刚生效（冲突后重读到了它）。
		if st.Captured(captureID) {
			res = &CaptureResult{Duplicate: true}
			return session.Decision{}, nil
		}
		if st.Closed != nil {
			return session.Decision{}, fmt.Errorf("%w: session %s is closed", ErrConflict, sessionID)
		}
		// 旁听与输入一样算作活动：整天只有旁听的 Session 不能被按空闲时长关闭。上一次活动取最近的 Run 与最近一批旁听中
		// 较晚的一个（写入之前的投影），按 TouchEvery 分段更新索引。
		prev = lastRunAt(st)
		if st.Captures.LastAt.After(prev) {
			prev = st.Captures.LastAt
		}
		// 不唤醒：旁听不开启 Run，没有需要推进的工作。
		return session.Decision{Events: []*v1.Event{{Payload: &v1.Event_CaptureAppended{CaptureAppended: m}}}}, nil
	})
	if err != nil {
		return nil, gone(sessionID, err)
	}
	if res.Duplicate {
		return res, nil
	}
	return res, s.touch(ctx, sessionID, prev)
}

// moderateCaptures 在写日志之前检查旁听转写（ADR-0021），返回违规的片段只留占位后的转写与拦截的段数。送检的单位是整批，
// 与一条输入相同：整批不违规就整批写入，不再逐段调用提供商。整批违规时逐段送检，只把单独违规的片段换成占位，
// 而不是拒绝整批（一句话不该让一批都丢失）；没有一段单独违规（违规取决于上下文）时整批只留占位。提供商不可用时整批拒绝，
// HostApp 稍后以同一 capture_id 重试。
func (s *Service) moderateCaptures(ctx context.Context, st *session.State, segments []*v1.CaptureSegment) ([]*v1.CaptureSegment, int, error) {
	check := func(text string) (bool, error) {
		v, err := moderation.Check(ctx, s.Moderator, moderation.Request{Stage: moderation.Input,
			BusinessLine: st.Created.GetBusinessLine(), SessionID: st.SessionID, Text: text})
		return v.Block, err
	}
	block, err := check(CaptureText(segments))
	if err != nil || !block {
		return segments, 0, err
	}
	blocked := make([]bool, len(segments))
	n := 0
	for i, g := range segments {
		if blocked[i], err = check(g.GetText()); err != nil {
			return nil, 0, err
		}
		if blocked[i] {
			n++
		}
	}
	out := make([]*v1.CaptureSegment, len(segments))
	for i, g := range segments {
		if blocked[i] || n == 0 {
			g = proto.Clone(g).(*v1.CaptureSegment)
			g.Text, g.Redacted = "", true
		}
		out[i] = g
	}
	if n == 0 {
		n = len(segments)
	}
	return out, n, nil
}

// CaptureText 是整批送检的文本：各段文字按顺序换行相连。模拟测试据此检查日志中没有本应拦截的旁听。
func CaptureText(segments []*v1.CaptureSegment) string {
	texts := make([]string, len(segments))
	for i, g := range segments {
		texts[i] = g.GetText()
	}
	return strings.Join(texts, "\n")
}
