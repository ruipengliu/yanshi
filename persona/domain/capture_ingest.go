package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"time"
)

var (
	ErrCaptureResync   = errors.New("capture generation requires resynchronization")
	ErrCaptureConflict = errors.New("capture sequence or identity conflicts")
	ErrCaptureInvalid  = errors.New("invalid capture batch")
)

// OrderedCapture 的序号属于采集会话，引用编号仍属于业务日；跨午夜不能混用两种编号。
type OrderedCapture struct {
	SessionID   string           `json:"session_id"`
	Seq         uint64           `json:"seq"`
	CaptureID   string           `json:"capture_id"`
	BusinessDay string           `json:"business_day"`
	TimeZone    string           `json:"time_zone"`
	Generation  uint64           `json:"generation"`
	Segments    []CaptureSegment `json:"segments"`
}

type CaptureReceipt struct {
	Status      string   `json:"status"`
	BusinessDay string   `json:"business_day"`
	Generation  uint64   `json:"generation"`
	NextSeq     uint64   `json:"next_seq"`
	Refs        []string `json:"refs,omitempty"`
}

type captureSubmission struct {
	Date       string
	Generation uint64
	CaptureID  string
	Digest     string
	Refs       []string
}

type captureStream struct {
	DeviceID    string
	Next        uint64
	Closed      bool
	Invalidated bool
	Batches     map[uint64]captureSubmission
}

// CaptureStaging 是原文库的暂存区，与 CaptureAccess 使用同一 owner 事务。
// nil 正文表示删除；ClearPending 只清指定业务日，不能把正文带进 Book 快照。
type CaptureStaging interface {
	Pending(date, session string, seq, generation uint64) ([]CaptureSegment, error)
	PutPending(date, session string, seq, generation uint64, segments []CaptureSegment) error
	ClearPending(date string) error
}

// NormalizeOrderedCapture 在暂存前完整校验，坏批次不能堵住后续排空。
func NormalizeOrderedCapture(in OrderedCapture) (OrderedCapture, error) {
	if in.SessionID == "" || len(in.SessionID) > 128 || in.Seq == 0 || in.Seq == math.MaxUint64 {
		return in, ErrCaptureInvalid
	}
	loc, ok := captureZone(in.TimeZone)
	if !ok || in.TimeZone == "" {
		return in, ErrCaptureInvalid
	}
	in.Segments = CloneCaptureSegments(in.Segments)
	for i := range in.Segments {
		g := &in.Segments[i]
		at, err := time.Parse(time.RFC3339Nano, g.SpokenAt)
		if err != nil {
			return in, ErrCaptureInvalid
		}
		date := at.In(loc).Format(time.DateOnly)
		if in.BusinessDay == "" {
			in.BusinessDay = date
		}
		if in.BusinessDay != date {
			return in, ErrCaptureInvalid
		}
		g.TimeZone = in.TimeZone
	}
	if _, err := CaptureExpiresAt(in.BusinessDay, in.TimeZone); err != nil {
		return in, ErrCaptureInvalid
	}
	if validateCaptureBatch(in.CaptureID, in.Segments) != nil {
		return in, ErrCaptureInvalid
	}
	return in, nil
}

// CaptureSync 只读控制状态；读取新世代不授权客户端给旧正文换世代。
func (u *Book) CaptureSync(date, session, device string) (CaptureReceipt, error) {
	if _, err := time.Parse(time.DateOnly, date); err != nil {
		return CaptureReceipt{}, ErrCaptureInvalid
	}
	out := CaptureReceipt{Status: "ready", BusinessDay: date, NextSeq: 1}
	if d := u.captureDays[date]; d != nil {
		out.Generation = d.CaptureGeneration
	}
	if s := u.captureStreams[session]; s != nil {
		if s.DeviceID != device {
			return CaptureReceipt{}, ErrCaptureConflict
		}
		out.NextSeq = s.Next
		if s.Closed {
			out.Status = "closed"
		}
		if s.Invalidated {
			out.Status = "resync_required"
		}
	}
	if err := u.CheckCaptures(date); err != nil {
		return out, err
	}
	return out, nil
}

// ReceiveCaptures 与排空在一个 owner 事务中提交；只有 AppendCaptures 分配引用。
func (u *Book) ReceiveCaptures(device string, in OrderedCapture) (CaptureReceipt, error) {
	var err error
	in, err = NormalizeOrderedCapture(in)
	if err != nil || device == "" {
		return CaptureReceipt{}, ErrCaptureInvalid
	}
	out, err := u.CaptureSync(in.BusinessDay, in.SessionID, device)
	if errors.Is(err, ErrCaptureConflict) || errors.Is(err, ErrCaptureInvalid) {
		return out, err
	}
	// 世代先于删除状态检查，使旧请求明确得到重新同步而不是普通冲突。
	if out.Generation != in.Generation || out.Status == "resync_required" {
		return out, ErrCaptureResync
	}
	if err != nil {
		return out, err
	}
	access, err := u.captureAccess()
	if err != nil {
		return out, err
	}
	day := u.captureDay(in.BusinessDay)
	if day.ExpiresAt.IsZero() {
		day.TimeZone = in.TimeZone
		day.ExpiresAt, _ = CaptureExpiresAt(in.BusinessDay, in.TimeZone)
		day.DeleteAt = day.ExpiresAt
	}
	if err := u.CheckCaptures(in.BusinessDay); err != nil {
		return out, err
	}
	s := u.captureStreams[in.SessionID]
	if s == nil {
		for _, active := range u.captureStreams {
			if !active.Closed && !active.Invalidated {
				return out, ErrCaptureConflict
			}
		}
		s = &captureStream{DeviceID: device, Next: 1, Batches: map[uint64]captureSubmission{}}
		if u.captureStreams == nil {
			u.captureStreams = map[string]*captureStream{}
		}
		u.captureStreams[in.SessionID] = s
	}
	data, _ := json.Marshal(in)
	hash := sha256.Sum256(data)
	digest := hex.EncodeToString(hash[:])
	if prior, exists := s.Batches[in.Seq]; exists {
		if prior.Digest != digest {
			return out, ErrCaptureConflict
		}
		if len(prior.Refs) > 0 {
			out.Status, out.Refs, out.NextSeq = "accepted", slices.Clone(prior.Refs), s.Next
			return out, nil
		}
	} else {
		if s.Closed || in.Seq < s.Next {
			return out, ErrCaptureConflict
		}
		// capture_id 在业务日内唯一，不能用新序号把同一批次插入两次。
		for _, stream := range u.captureStreams {
			for _, prior := range stream.Batches {
				if prior.Date == in.BusinessDay && prior.CaptureID == in.CaptureID {
					return out, ErrCaptureConflict
				}
			}
		}
		if _, exists := day.BatchIDs[in.CaptureID]; exists {
			return out, ErrCaptureConflict
		}
		if err := access.PutPending(in.BusinessDay, in.SessionID, in.Seq, in.Generation, in.Segments); err != nil {
			return out, err
		}
		s.Batches[in.Seq] = captureSubmission{Date: in.BusinessDay, Generation: in.Generation, CaptureID: in.CaptureID, Digest: digest}
	}
	for {
		pending, exists := s.Batches[s.Next]
		if !exists {
			break
		}
		d := u.captureDays[pending.Date]
		if d == nil || d.CaptureGeneration != pending.Generation || u.CheckCaptures(pending.Date) != nil {
			s.Invalidated = true
			break
		}
		segments, err := access.Pending(pending.Date, in.SessionID, s.Next, pending.Generation)
		if err != nil {
			return out, err
		}
		refs, err := u.AppendCaptures(pending.Date, pending.CaptureID, segments)
		if err != nil {
			return out, err
		}
		if err := access.PutPending(pending.Date, in.SessionID, s.Next, pending.Generation, nil); err != nil {
			return out, err
		}
		pending.Refs = refs
		s.Batches[s.Next] = pending
		s.Next++
	}
	out.Status, out.NextSeq = "staged", s.Next
	if s.Invalidated {
		out.Status = "resync_required"
	}
	if refs := s.Batches[in.Seq].Refs; len(refs) > 0 {
		out.Status, out.Refs = "accepted", slices.Clone(refs)
	}
	return out, nil
}

// CloseCaptureSession 只结束没有缺口的会话；失效会话允许结束但不会恢复旧批次。
func (u *Book) CloseCaptureSession(session, device string) error {
	s := u.captureStreams[session]
	if s == nil || s.DeviceID != device {
		return ErrCaptureConflict
	}
	if !s.Invalidated {
		for seq := range s.Batches {
			if seq >= s.Next {
				return ErrCaptureConflict
			}
		}
	}
	s.Closed = true
	return nil
}

func (u *Book) invalidateCaptureGeneration(date string) error {
	d := u.captureDays[date]
	if d.CaptureGeneration == math.MaxUint64 {
		return ErrCaptureConflict
	}
	if u.captures == nil {
		return ErrCaptureTransactionClosed
	}
	if err := u.captures.ClearPending(date); err != nil {
		return err
	}
	d.CaptureGeneration++
	u.invalidateCaptureStreams(date)
	return nil
}

func (u *Book) invalidateCaptureStreams(date string) {
	for _, stream := range u.captureStreams {
		for _, batch := range stream.Batches {
			if batch.Date == date {
				stream.Invalidated = true
				break
			}
		}
	}
}
