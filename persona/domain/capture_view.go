package domain

import (
	"fmt"
	"iter"
)

// CaptureView 保留本人查询所需的真实元数据；不能直接作为可信页交给模型。
type CaptureView struct {
	Ref string `json:"ref"`
	CaptureSegment
}

func (g CaptureSegment) Visible() bool {
	return g.EffectiveDisposition() == "accept" && !g.Background
}

// ProjectCapture 是可信页与实况简述共用的信任边界；隐藏原文不参与分页或预算。
func ProjectCapture(ref Ref, g CaptureSegment) (PageSegment, bool) {
	if !g.Visible() {
		return PageSegment{}, false
	}
	p := PageSegment{Ref: ref.String(), Speaker: g.Speaker, SpeakerKey: g.SpeakerKey, SpeakerRef: g.SpeakerRef,
		Text: g.Text, SpokenAt: g.SpokenAt, TimeZone: g.TimeZone}
	if g.Speaker == "unknown" || g.Speaker == "wearer" && g.EffectiveSpeakerGrade() != "evidence" {
		// 批次至多 20 位、段号至多 3 位；逐段分配，避免把不确定身份关联成同一个人。
		p.Speaker, p.SpeakerKey, p.SpeakerRef = "other", fmt.Sprintf("u-%d-%d", ref.Seq, ref.Index), ""
	}
	return p, true
}

func (p PageSegment) captureSegment() captureSegment {
	ref, _ := parseCaptureRef(p.Ref)
	return captureSegment{ref: ref, g: CaptureSegment{Speaker: p.Speaker, SpeakerKey: p.SpeakerKey, SpeakerRef: p.SpeakerRef,
		Text: p.Text, SpokenAt: p.SpokenAt, TimeZone: p.TimeZone}}
}

func (u *Book) visibleCaptureSegments(date string, from, to Ref) iter.Seq2[captureSegment, error] {
	return func(yield func(captureSegment, error) bool) {
		for s, err := range u.captureSegments(date, from, to) {
			if err != nil {
				yield(captureSegment{}, err)
				return
			}
			if p, visible := ProjectCapture(s.ref, s.g); visible && !yield(p.captureSegment(), nil) {
				return
			}
		}
	}
}

func (u *Book) visibleCaptureAdjacent(date string, ref Ref, before bool) (CaptureRecord, bool, error) {
	access, err := u.captureAccess()
	if err != nil {
		return CaptureRecord{}, false, err
	}
	for {
		r, found, err := access.Adjacent(date, ref, before)
		if err != nil || !found {
			return CaptureRecord{}, false, err
		}
		if r.Segment.Visible() {
			return r, true, nil
		}
		ref = r.Ref
	}
}

// 旧日只升级一次；必须在更新接收事实之前保留旧静默起点。
func (u *Book) prepareVisibleCaptures(date string) error {
	d := u.captureDays[date]
	if d == nil || d.VisibleTail != nil {
		return nil
	}
	last, _, err := u.visibleCaptureAdjacent(date, Ref{}, true)
	if err != nil {
		return err
	}
	d.VisibleTail = &last.Ref
	d.LastVisibleBatchAt = d.LastBatchAt
	return nil
}
