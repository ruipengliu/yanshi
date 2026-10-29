package devui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"xinli/kernel"

	"persona/domain"
	"persona/store"
)

type Options struct {
	Store       store.Store
	DaySessions interface {
		EnsureDaySession(context.Context, string, string, string) (string, error)
	}
	Kernel interface {
		Submit(context.Context, string, string, string) error
		OneshotForBusinessLine(context.Context, string, string, string, string) (kernel.OneshotResult, error)
	}
	Notifier interface {
		DeletionPending(context.Context, string, string, string)
	}
	Now func() time.Time
}

func registerRoutes(mux *http.ServeMux, opts Options) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	prefix := "/v1/dev/{businessLine}/{endUser}"
	write := func(w http.ResponseWriter, status int, out any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(out)
	}
	fail := func(w http.ResponseWriter, status int, class string) {
		write(w, status, map[string]string{"error": class})
	}
	decode := func(w http.ResponseWriter, r *http.Request, out any) bool {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
		dec.DisallowUnknownFields()
		if err := dec.Decode(out); err != nil {
			fail(w, 400, "invalid_request")
			return false
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			fail(w, 400, "invalid_request")
			return false
		}
		return true
	}
	validDate := func(date string) bool { _, err := time.Parse(time.DateOnly, date); return err == nil }
	mux.HandleFunc("GET "+prefix+"/captures", func(w http.ResponseWriter, r *http.Request) {
		date := r.URL.Query().Get("date")
		if !validDate(date) {
			fail(w, 400, "invalid_date")
			return
		}
		var out struct {
			Day      domain.BusinessDay    `json:"day"`
			Segments []domain.PageSegment  `json:"segments"`
			Tasks    []domain.DeletionTask `json:"tasks"`
		}
		err := opts.Store.View(r.Context(), r.PathValue("businessLine"), r.PathValue("endUser"), func(b *domain.Book) error {
			b.SetCaptureClock(opts.Now)
			out.Day = b.BusinessDay(date)
			var err error
			out.Segments, err = b.DayCaptures(date)
			if err != nil {
				return err
			}
			out.Tasks = []domain.DeletionTask{}
			for _, task := range b.DeletionTasks() {
				if task.Date == date {
					out.Tasks = append(out.Tasks, task)
				}
			}
			return nil
		})
		if err != nil {
			if errors.Is(err, domain.ErrCapturesExpired) {
				write(w, 409, map[string]any{"error": err.Error(), "day": out.Day})
			} else {
				fail(w, 500, "read_failed")
			}
			return
		}
		write(w, 200, out)
	})
	mux.HandleFunc("POST "+prefix+"/captures", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Date     string                  `json:"date"`
			Segments []domain.CaptureSegment `json:"segments"`
		}
		if !decode(w, r, &in) {
			return
		}
		id, err := randomID()
		if err != nil {
			fail(w, 500, "id_failed")
			return
		}
		var refs []string
		err = opts.Store.Update(r.Context(), r.PathValue("businessLine"), r.PathValue("endUser"), func(b *domain.Book) error {
			b.SetCaptureClock(opts.Now)
			var err error
			refs, err = b.AppendCaptures(in.Date, id, in.Segments)
			return err
		})
		if err != nil {
			if errors.Is(err, domain.ErrCapturesExpired) {
				fail(w, 409, err.Error())
			} else {
				fail(w, 400, "capture_rejected")
			}
			return
		}
		write(w, 200, map[string]any{"refs": refs})
	})
	for _, action := range []string{"erase", "erase_day"} {
		mux.HandleFunc("POST "+prefix+"/"+action, func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				Date string `json:"date"`
				From string `json:"from"`
				To   string `json:"to"`
			}
			if !decode(w, r, &in) {
				return
			}
			var task domain.DeletionTask
			bl, eu := r.PathValue("businessLine"), r.PathValue("endUser")
			err := opts.Store.Update(r.Context(), bl, eu, func(b *domain.Book) error {
				b.SetCaptureClock(opts.Now)
				var err error
				if action == "erase" {
					task, err = b.Erase(in.Date, in.From, in.To, opts.Now())
				} else {
					task, err = b.EraseDay(in.Date, opts.Now())
				}
				return err
			})
			if err != nil {
				fail(w, 400, "erase_rejected")
				return
			}
			if opts.Notifier != nil {
				opts.Notifier.DeletionPending(r.Context(), bl, eu, task.ID)
			}
			write(w, 200, map[string]any{"task": task})
		})
	}
	mux.HandleFunc("POST "+prefix+"/brief", func(w http.ResponseWriter, r *http.Request) {
		if opts.Kernel == nil {
			fail(w, 503, "kernel_disabled")
			return
		}
		var in struct {
			Date string `json:"date"`
		}
		if !decode(w, r, &in) {
			return
		}
		if !validDate(in.Date) {
			fail(w, 400, "invalid_date")
			return
		}
		bl, eu := r.PathValue("businessLine"), r.PathValue("endUser")
		var before briefRevision
		var input string
		err := opts.Store.View(r.Context(), bl, eu, func(b *domain.Book) error {
			b.SetCaptureClock(opts.Now)
			segments, err := b.DayCaptures(in.Date)
			if err != nil {
				return err
			}
			before = captureRevision(b, in.Date)
			input = briefInput(segments)
			return nil
		})
		if err != nil {
			if errors.Is(err, domain.ErrCapturesExpired) {
				fail(w, 409, err.Error())
			} else {
				fail(w, 500, "read_failed")
			}
			return
		}
		if before.deleted || input == "" {
			fail(w, 409, "no_captures")
			return
		}
		result, callErr := opts.Kernel.OneshotForBusinessLine(r.Context(), bl, eu, "xinli-live-brief", input)
		var after briefRevision
		err = opts.Store.View(r.Context(), bl, eu, func(b *domain.Book) error {
			b.SetCaptureClock(opts.Now)
			if err := b.CheckCaptures(in.Date); errors.Is(err, domain.ErrCapturesExpired) {
				return err
			}
			after = captureRevision(b, in.Date)
			return nil
		})
		if err != nil {
			if errors.Is(err, domain.ErrCapturesExpired) {
				fail(w, 409, err.Error())
			} else {
				fail(w, 500, "read_failed")
			}
			return
		}
		if before != after {
			fail(w, 409, "原文已变化，请重新生成")
			return
		}
		if callErr != nil {
			status := http.StatusBadGateway
			var remote *kernel.HTTPError
			if errors.As(callErr, &remote) {
				switch remote.StatusCode {
				case 429, 503, 504:
					status = remote.StatusCode
				case 422:
					status = http.StatusUnprocessableEntity
				}
			} else if errors.Is(callErr, context.DeadlineExceeded) {
				status = http.StatusGatewayTimeout
			}
			fail(w, status, "brief_failed")
			return
		}
		var output struct {
			Brief *string `json:"brief"`
		}
		if json.Unmarshal(result.Output, &output) != nil || output.Brief == nil {
			fail(w, 502, "invalid_brief")
			return
		}
		write(w, 200, map[string]string{"brief": *output.Brief})
	})
	mux.HandleFunc("POST "+prefix+"/organize", func(w http.ResponseWriter, r *http.Request) {
		if opts.DaySessions == nil || opts.Kernel == nil {
			fail(w, 503, "orchestrator_disabled")
			return
		}
		var in struct {
			Date    string `json:"date"`
			Kind    string `json:"kind"`
			InputID string `json:"input_id"`
		}
		if !decode(w, r, &in) {
			return
		}
		if !validDate(in.Date) || (in.Kind != "exchange" && in.Kind != "day") || len(in.InputID) > 128 {
			fail(w, 400, "invalid_request")
			return
		}
		text := "整理今天"
		if in.Kind == "exchange" {
			text = "整理交谈"
		}
		if in.InputID == "" {
			var err error
			in.InputID, err = randomID()
			if err != nil {
				fail(w, 500, "id_failed")
				return
			}
		}
		sid, err := opts.DaySessions.EnsureDaySession(r.Context(), r.PathValue("businessLine"), r.PathValue("endUser"), in.Date)
		if err != nil {
			if errors.Is(err, domain.ErrCapturesExpired) {
				fail(w, 409, err.Error())
			} else if errors.Is(err, domain.ErrDayDeleted) {
				fail(w, 409, "day_deleted")
			} else {
				fail(w, 502, "ensure_day_failed")
			}
			return
		}
		if err := opts.Kernel.Submit(r.Context(), sid, in.InputID, text); err != nil {
			fail(w, 502, "submit_failed")
			return
		}
		write(w, 200, map[string]string{"session_id": sid, "input_id": in.InputID})
	})
}

func randomID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "xinli_dev_" + hex.EncodeToString(buf[:]), nil
}
