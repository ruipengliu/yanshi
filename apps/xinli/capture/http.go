// Package capture 是认证后的文字采集入口；顺序与删除规则只由 Persona 决定。
package capture

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"persona/domain"
	"persona/store"
	"xinli/account"
)

type Segment struct {
	Speaker    string `json:"speaker"`
	Text       string `json:"text"`
	At         string `json:"at"`
	SpeakerRef string `json:"speaker_ref,omitempty"`
}

type Batch struct {
	SessionID   string    `json:"session_id"`
	Seq         uint64    `json:"seq"`
	CaptureID   string    `json:"capture_id"`
	BusinessDay string    `json:"business_day,omitempty"`
	TimeZone    string    `json:"time_zone"`
	Generation  uint64    `json:"generation"`
	Segments    []Segment `json:"segments"`
}

type Receipt = domain.CaptureReceipt

type Error struct {
	Code string `json:"code"`
	Receipt
}

// Ordered 只转换线上字段，不复制 Persona 的时区、批次和内容校验规则。
func (b Batch) Ordered() domain.OrderedCapture {
	out := domain.OrderedCapture{SessionID: b.SessionID, Seq: b.Seq, CaptureID: b.CaptureID, BusinessDay: b.BusinessDay, TimeZone: b.TimeZone, Generation: b.Generation}
	for _, segment := range b.Segments {
		speaker, key := segment.Speaker, ""
		if speaker == "other" || strings.HasPrefix(speaker, "other-") {
			key, speaker = speaker, "other"
		}
		out.Segments = append(out.Segments, domain.CaptureSegment{Speaker: speaker, SpeakerKey: key, SpeakerRef: segment.SpeakerRef, Text: segment.Text, SpokenAt: segment.At, TimeZone: b.TimeZone})
	}
	return out
}

func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func failure(w http.ResponseWriter, out Receipt, err error) {
	// 提交失败时不能把回调内已分配的引用伪装成已接受回执。
	if !errors.Is(err, domain.ErrCaptureResync) {
		out = Receipt{}
	}
	code, status := "unavailable", http.StatusServiceUnavailable
	switch {
	case errors.Is(err, domain.ErrCaptureResync):
		code, status = "resync_required", http.StatusConflict
		out.Status = code
	case errors.Is(err, domain.ErrCaptureConflict):
		code, status = "capture_conflict", http.StatusConflict
	case errors.Is(err, domain.ErrCaptureInvalid):
		code, status = "invalid_batch", http.StatusBadRequest
	case errors.Is(err, domain.ErrCapturesExpired):
		code, status = "expired", http.StatusGone
	case errors.Is(err, domain.ErrDayDeleted):
		code, status = "day_deleted", http.StatusGone
	case errors.Is(err, store.ErrOwnerDeleting):
		code, status = "account_inactive", http.StatusForbidden
	}
	jsonResponse(w, status, Error{Code: code, Receipt: out})
}

// Handler 必须经 A12 登录态进入，owner 与安装实例只从可信上下文读取。
func Handler(books store.Store, accounts *account.Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/captures/batches", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Batch
			Generation *uint64 `json:"generation"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&in) != nil || decoder.Decode(new(any)) != io.EOF || in.Generation == nil {
			failure(w, Receipt{}, domain.ErrCaptureInvalid)
			return
		}
		in.Batch.Generation = *in.Generation
		p, _ := account.FromContext(r.Context())
		var out Receipt
		err := books.Update(r.Context(), p.Owner.BusinessLine, p.Owner.EndUser, func(b *domain.Book) error {
			var err error
			out, err = b.ReceiveCaptures(p.DeviceID, in.Batch.Ordered())
			return err
		})
		if err != nil {
			failure(w, out, err)
			return
		}
		if out.Status == "resync_required" {
			failure(w, out, domain.ErrCaptureResync)
			return
		}
		status := http.StatusOK
		if out.Status == "staged" {
			status = http.StatusAccepted
		}
		jsonResponse(w, status, out)
	})
	mux.HandleFunc("GET /v1/captures/sync", func(w http.ResponseWriter, r *http.Request) {
		p, _ := account.FromContext(r.Context())
		var out Receipt
		err := books.View(r.Context(), p.Owner.BusinessLine, p.Owner.EndUser, func(b *domain.Book) error {
			var err error
			out, err = b.CaptureSync(r.URL.Query().Get("business_day"), r.URL.Query().Get("session_id"), p.DeviceID)
			return err
		})
		if err != nil {
			failure(w, out, err)
			return
		}
		jsonResponse(w, http.StatusOK, out)
	})
	mux.HandleFunc("POST /v1/captures/sessions/{id}/close", func(w http.ResponseWriter, r *http.Request) {
		p, _ := account.FromContext(r.Context())
		err := books.Update(r.Context(), p.Owner.BusinessLine, p.Owner.EndUser, func(b *domain.Book) error {
			return b.CloseCaptureSession(r.PathValue("id"), p.DeviceID)
		})
		if err != nil {
			failure(w, Receipt{}, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return accounts.Middleware(mux)
}
