package service

import (
	"context"
	"errors"
	"fmt"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/card"
	"yanshi/internal/session"
)

// cardAction 核对输入中的卡片动作（docs/design/cards.md §4）：动作须属于本 Session 日志中的那一版卡片。返回规范化后的
// 输入与推导出的输入 ID；输入中没有卡片动作时原样返回、ID 为空。用户输入中不能带卡片本身（卡片只由 show_card 写入）。
func (s *Service) cardAction(ctx context.Context, st *session.State, input []*v1.ContentBlock) (string, []*v1.ContentBlock, error) {
	idx := -1
	for i, b := range input {
		if b.GetCard() != nil {
			return "", nil, fmt.Errorf("%w: cards are written by show_card, not submitted", ErrInvalid)
		}
		if b.GetCardAction() != nil {
			if idx >= 0 {
				return "", nil, fmt.Errorf("%w: at most one card_action per input", ErrInvalid)
			}
			idx = i
		}
	}
	if idx < 0 {
		return "", input, nil
	}
	a := input[idx].GetCardAction()
	c, err := s.findCard(ctx, st, a)
	if err != nil {
		return "", nil, err
	}
	checked, err := card.CheckAction(c, a)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	out := append([]*v1.ContentBlock(nil), input...)
	out[idx] = &v1.ContentBlock{Kind: &v1.ContentBlock_CardAction{CardAction: checked}}
	return card.InputID(checked), out, nil
}

// errCardFound 结束日志扫描。
var errCardFound = errors.New("card found")

// findCard 读取动作所指的那一版卡片：客户端给出了收到卡片的事件序号时直接读那一条，否则扫描日志（罕见：旧客户端）。
func (s *Service) findCard(ctx context.Context, st *session.State, a *v1.CardAction) (*v1.Card, error) {
	match := func(e *v1.Event) *v1.Card {
		r := e.GetToolResult()
		if r == nil || r.GetCallId() != a.GetCallId() || r.GetAttempt() == 0 {
			return nil
		}
		if c := card.Find(r.GetContent()); c != nil && c.GetCardId() == a.GetCardId() {
			return c
		}
		return nil
	}
	if seq := a.GetSeq(); seq > 0 && seq <= st.Seq {
		page, err := s.Store.Log.Read(ctx, st.SessionID, seq-1, 1)
		if err != nil {
			return nil, err
		}
		if len(page) == 1 {
			if c := match(page[0]); c != nil {
				return c, nil
			}
		}
	}
	var found *v1.Card
	err := session.ScanLog(ctx, s.Store.Log, st.SessionID, func(e *v1.Event) bool {
		found = match(e)
		return found == nil && e.GetSeq() < st.Seq
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, fmt.Errorf("%w: card %s (call %s) in session %s", ErrNotFound, a.GetCardId(), a.GetCallId(), st.SessionID)
	}
	return found, nil
}
