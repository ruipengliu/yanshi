package oneshot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/clock"
	"yanshi/internal/ids"
	"yanshi/internal/lifecycle"
	"yanshi/internal/metrics"
	"yanshi/internal/model"
	"yanshi/internal/moderation"
	"yanshi/internal/usage"
)

type providerFunc func(context.Context, *model.Request) (*model.Response, error)

func (f providerFunc) Generate(ctx context.Context, req *model.Request, _ func(model.Delta)) (*model.Response, error) {
	return f(ctx, req)
}

type moderatorFunc func(context.Context, moderation.Request) (moderation.Verdict, error)

func (f moderatorFunc) Check(ctx context.Context, req moderation.Request) (moderation.Verdict, error) {
	return f(ctx, req)
}

func response(text string) *model.Response {
	return &model.Response{Content: model.TextBlocks(text), Usage: &v1.Usage{InputTokens: 5, CachedInputTokens: 2, OutputTokens: 3}}
}
func request() Request {
	return Request{BusinessLine: "bl", EndUser: "u", Name: "brief", Input: "你好"}
}

func serviceFor(t *testing.T, defs ...*Def) (*Service, *usage.Mem) {
	t.Helper()
	if len(defs) == 0 {
		defs = []*Def{definition()}
	}
	r, err := NewRegistry(defs...)
	if err != nil {
		t.Fatal(err)
	}
	uses, deletions := usage.NewMem(), lifecycle.NewMemDeletions()
	return &Service{Registry: r, Model: providerFunc(func(context.Context, *model.Request) (*model.Response, error) { return response("好"), nil }),
		Clock: clock.Real{}, IDs: ids.Sequential("usage"), Deletions: deletions, Moderator: moderation.Mock{}, Max: 4, Wait: 0, Deadline: time.Second,
		Meter: &usage.Meter{Store: uses, Deletions: deletions, Prices: &usage.PriceList{Models: map[string]usage.Price{"fake/model": {Input: 1, Output: 1}}}}}, uses
}

func records(t *testing.T, store usage.Store, by usage.GroupBy) []usage.Row {
	t.Helper()
	now := time.Now()
	rows, err := store.Report(context.Background(), usage.Query{BusinessLine: "bl", From: now.Add(-time.Hour), To: now.Add(time.Hour), GroupBy: by})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func markDeleted(t *testing.T, s *Service) {
	t.Helper()
	if _, err := s.Deletions.MarkEndUser(context.Background(), &lifecycle.EndUserTombstone{BusinessLine: "bl", EndUser: "u", RequestID: "delete", RequestedAt: s.Clock.Now()}); err != nil {
		t.Fatal(err)
	}
}

func TestCallChecksBeforeModel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*Service, *Request)
		want  error
	}{
		{"unknown", func(_ *Service, r *Request) { r.Name = "missing" }, ErrNotFound},
		{"empty", func(_ *Service, r *Request) { r.Input = " \n" }, ErrInvalid},
		{"long", func(_ *Service, r *Request) { r.Input = strings.Repeat("中", 101) }, ErrInvalid},
		{"end_user", func(_ *Service, r *Request) { r.EndUser = "" }, ErrInvalid},
		{"anonymous", func(_ *Service, r *Request) { r.EndUser = usage.Anonymous }, ErrInvalid},
		{"business_line", func(_ *Service, r *Request) { r.BusinessLine = "" }, ErrInvalid},
		{"barrier", func(s *Service, _ *Request) { markDeleted(t, s) }, ErrNotFound},
		{"moderation", func(_ *Service, r *Request) { r.Input = "【违规测试】" }, moderation.ErrRejected},
		{"moderation_unavailable", func(s *Service, _ *Request) {
			s.Moderator = moderatorFunc(func(context.Context, moderation.Request) (moderation.Verdict, error) {
				return moderation.Verdict{}, errors.New("private input")
			})
		}, moderation.ErrUnavailable},
		{"quota", func(s *Service, _ *Request) {
			uses := usage.NewMem()
			if err := uses.Record(context.Background(), &usage.Entry{ID: "before", BusinessLine: "bl", EndUser: "u", Kind: usage.Model, Cost: 2_000_000, At: s.Clock.Now()}); err != nil {
				t.Fatal(err)
			}
			s.Quotas = &usage.Quotas{Store: uses, Limits: map[string]usage.Limits{"bl": {EndUserDaily: 1}}}
		}, usage.ErrExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, uses := serviceFor(t)
			s.Model = providerFunc(func(context.Context, *model.Request) (*model.Response, error) {
				t.Fatal("model called after rejection")
				return nil, nil
			})
			req := request()
			tc.setup(s, &req)
			_, err := s.Call(context.Background(), req)
			if !errors.Is(err, tc.want) || strings.Contains(err.Error(), "private") {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if len(records(t, uses, usage.ByOneshot)) != 0 {
				t.Fatal("rejection billed")
			}
		})
	}
	s, _ := serviceFor(t)
	req := request()
	req.Input = strings.Repeat("中", 100)
	if _, err := s.Call(context.Background(), req); err != nil {
		t.Fatal("Unicode count rejected", err)
	}
}

func TestCallSchemaRepairPinsVersionAndMetersBoth(t *testing.T) {
	for _, repaired := range []bool{true, false} {
		t.Run(fmt.Sprint(repaired), func(t *testing.T) {
			a, b := schemaDef(), schemaDef()
			b.Version, b.Model, b.Instructions = "2", "fake/new", "新版本"
			s, uses := serviceFor(t, a, b)
			set := func(v string) {
				rel, err := s.Registry.ParseReleases([]byte("brief: {stable: \"" + v + "\"}"))
				if err != nil {
					t.Fatal(err)
				}
				s.Registry.SetReleases(rel)
			}
			set("1")
			calls := 0
			s.Model = providerFunc(func(ctx context.Context, req *model.Request) (*model.Response, error) {
				calls++
				if req.Model != "fake/model" || req.MaxTokens != 50 || len(req.Tools) != 0 || len(req.Messages) != 1 || model.Text(req.Messages[0].Content) != "你好" {
					t.Fatalf("request changed: %+v", req)
				}
				if !strings.Contains(req.System, SchemaInstruction) || strings.Contains(req.System, "private-output") {
					t.Fatal("unsafe schema prompt")
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("missing shared deadline")
				}
				if calls == 1 {
					set("2")
					return response(`{"brief":99,"private-output":"private-output"}`), nil
				}
				if !strings.Contains(req.System, "schema_mismatch") {
					t.Fatal("missing repair category")
				}
				if repaired {
					return response(`{"brief":"好"}`), nil
				}
				return response(`not json`), nil
			})
			result, err := s.Call(context.Background(), request())
			if repaired && err != nil || !repaired && !errors.Is(err, ErrSchema) {
				t.Fatalf("err: %v", err)
			}
			if calls != 2 || result.Version != "1" || result.Usage.GetInputTokens() != 10 {
				t.Fatalf("result: %+v, calls %d", result, calls)
			}
			if !repaired && result.Output != nil {
				t.Fatal("invalid output returned")
			}
			rows := records(t, uses, usage.ByOneshot)
			if len(rows) != 1 || rows[0].Key != "brief@1" || rows[0].Calls != 2 || rows[0].Cost != 16 {
				t.Fatalf("usage: %+v", rows)
			}
			if rows := records(t, uses, usage.ByAgent); len(rows) != 1 || rows[0].Key != "" {
				t.Fatal("oneshot masqueraded as agent", rows)
			}
		})
	}
}

func TestOutputRejectionDoesNotRepair(t *testing.T) {
	s, uses := serviceFor(t, schemaDef())
	calls := 0
	s.Model = providerFunc(func(context.Context, *model.Request) (*model.Response, error) {
		calls++
		return response(`{"brief":"【违规测试】"}`), nil
	})
	result, err := s.Call(context.Background(), request())
	if !errors.Is(err, moderation.ErrRejected) || calls != 1 || result.Output != nil {
		t.Fatalf("result %+v, err %v, calls %d", result, err, calls)
	}
	if rows := records(t, uses, usage.ByOneshot); len(rows) != 1 || rows[0].Calls != 1 {
		t.Fatal(rows)
	}
}

func TestCallDeletionInterleavings(t *testing.T) {
	for _, stage := range []string{"model", "record", "output"} {
		t.Run(stage, func(t *testing.T) {
			s, uses := serviceFor(t)
			erase := func() {
				markDeleted(t, s)
				if err := uses.DeleteEndUser(context.Background(), "bl", "u"); err != nil {
					t.Fatal(err)
				}
			}
			s.Model = providerFunc(func(context.Context, *model.Request) (*model.Response, error) {
				if stage == "model" {
					erase()
				}
				return response("private-output"), nil
			})
			if stage == "record" {
				s.Meter.Store = recordHook{Store: uses, before: erase}
			}
			s.Moderator = moderatorFunc(func(_ context.Context, req moderation.Request) (moderation.Verdict, error) {
				if stage == "output" && req.Stage == moderation.Output {
					erase()
				}
				return moderation.Verdict{}, nil
			})
			result, err := s.Call(context.Background(), request())
			if !errors.Is(err, ErrNotFound) || result.Output != nil {
				t.Fatalf("result: %+v, %v", result, err)
			}
			rows := records(t, uses, usage.ByEndUser)
			if len(rows) != 1 || rows[0].Key != usage.Anonymous || rows[0].InputTokens != 5 {
				t.Fatalf("late usage retained EndUser: %+v", rows)
			}
		})
	}
}

type recordHook struct {
	usage.Store
	before func()
}

func (s recordHook) Record(ctx context.Context, e *usage.Entry) error {
	s.before()
	return s.Store.Record(ctx, e)
}

func TestBusyAndCancellationReleaseAllLayers(t *testing.T) {
	for _, layer := range []string{"total", "business_line:bl", "name:bl:brief"} {
		t.Run(layer, func(t *testing.T) {
			s, _ := serviceFor(t)
			s.BusinessLineMax = map[string]int{"bl": 2}
			limits := map[string]int{"total": 4, "business_line:bl": 2, "name:bl:brief": 2}
			var held []func()
			for range limits[layer] {
				release, err := s.Slots.Acquire(context.Background(), layer, limits[layer], 0)
				if err != nil {
					t.Fatal(err)
				}
				held = append(held, release)
			}
			_, err := s.Call(context.Background(), request())
			if !errors.Is(err, model.ErrBusy) {
				t.Fatal("want busy", err)
			}
			s.Wait = time.Second
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			_, err = s.Call(ctx, request())
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("want deadline", err)
			}
			for _, release := range held {
				release()
			}
			// 三层都应完全归还，获取全部容量可发现部分获取后的泄漏。
			for key, limit := range limits {
				var releases []func()
				for range limit {
					release, err := s.Slots.Acquire(context.Background(), key, limit, 0)
					if err != nil {
						t.Fatalf("leaked %s: %v", key, err)
					}
					releases = append(releases, release)
				}
				for _, release := range releases {
					release()
				}
			}
		})
	}
}

func TestDeadlineCoversInputAndRepair(t *testing.T) {
	s, _ := serviceFor(t)
	s.Deadline = 10 * time.Millisecond
	s.Moderator = moderatorFunc(func(ctx context.Context, _ moderation.Request) (moderation.Verdict, error) {
		<-ctx.Done()
		return moderation.Verdict{}, ctx.Err()
	})
	if _, err := s.Call(context.Background(), request()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	s, _ = serviceFor(t, schemaDef())
	calls := 0
	var deadline time.Time
	s.Model = providerFunc(func(ctx context.Context, _ *model.Request) (*model.Response, error) {
		calls++
		d, _ := ctx.Deadline()
		if calls == 1 {
			deadline = d
			return response("bad JSON private-output"), nil
		}
		if !d.Equal(deadline) {
			t.Fatal("repair resets deadline")
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	s.Deadline = 10 * time.Millisecond
	if result, err := s.Call(context.Background(), request()); !errors.Is(err, context.DeadlineExceeded) || result.Output != nil || calls != 2 {
		t.Fatalf("%+v %v calls %d", result, err, calls)
	}
}

func TestContextAdmissionClass(t *testing.T) {
	for _, work := range []string{"interactive", "background"} {
		t.Run(work, func(t *testing.T) {
			d := definition()
			d.WorkClass = work
			s, _ := serviceFor(t, d)
			name := "oneshot-marker-" + work
			class := "oneshot"
			if work == "background" {
				class = "background_oneshot"
			}
			metric, err := metrics.ModelAdmissionWait.GetMetricWithLabelValues(name, class)
			if err != nil {
				t.Fatal(err)
			}
			before, after := &dto.Metric{}, &dto.Metric{}
			if err := metric.(interface{ Write(*dto.Metric) error }).Write(before); err != nil {
				t.Fatal(err)
			}
			s.Model = &model.Guard{Name: name, Provider: providerFunc(func(ctx context.Context, req *model.Request) (*model.Response, error) {
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("provider missing deadline")
				}
				return response("好"), nil
			})}
			if _, err := s.Call(context.Background(), request()); err != nil {
				t.Fatal(err)
			}
			if err := metric.(interface{ Write(*dto.Metric) error }).Write(after); err != nil {
				t.Fatal(err)
			}
			if after.GetHistogram().GetSampleCount() != before.GetHistogram().GetSampleCount()+1 {
				t.Fatal("Guard did not observe expected context markers")
			}
		})
	}
}

func TestValidateLimits(t *testing.T) {
	for _, tc := range []struct {
		max, provider int
		valid         bool
	}{{8, 0, true}, {8, 9, true}, {8, 8, false}, {8, 4, false}, {0, 0, false}, {-1, 0, false}} {
		if err := ValidateLimits(tc.max, tc.provider, DefaultWait, DefaultDeadline); (err == nil) != tc.valid {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
	if err := ValidateLimits(8, 0, -time.Second, time.Minute); err == nil {
		t.Fatal("negative wait accepted")
	}
	if err := ValidateLimits(8, 0, time.Second, 0); err == nil {
		t.Fatal("zero deadline accepted")
	}
}

// 遵守 ctx 的存储替身避免 Mem 忽略取消而掩盖真实 PostgreSQL 的行为。
type contextStore struct{ usage.Store }

func (s contextStore) Record(ctx context.Context, e *usage.Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.Store.Record(ctx, e)
}

func TestCancelAfterModelStillMetersWithinDeadlineAndReleases(t *testing.T) {
	s, uses := serviceFor(t)
	s.Meter.Store = contextStore{Store: uses}
	s.BusinessLineMax = map[string]int{"bl": 1}
	ctx, cancel := context.WithCancel(context.Background())
	s.Model = providerFunc(func(context.Context, *model.Request) (*model.Response, error) {
		cancel()
		return response("private-output"), context.Canceled
	})
	result, err := s.Call(ctx, request())
	if !errors.Is(err, context.Canceled) || result.Output != nil {
		t.Fatalf("%+v, %v", result, err)
	}
	if rows := records(t, uses, usage.ByOneshot); len(rows) != 1 || rows[0].InputTokens != 5 {
		t.Fatalf("canceled usage lost: %+v", rows)
	}
	s.Model = providerFunc(func(context.Context, *model.Request) (*model.Response, error) { return response("好"), nil })
	if _, err := s.Call(context.Background(), request()); err != nil {
		t.Fatal("slots not released", err)
	}
}

func TestDeclarationLimitSharedAcrossVersionsWithinBusinessLine(t *testing.T) {
	a, b := definition(), definition()
	a.Concurrency, b.Concurrency, b.Version = 1, 1, "2"
	s, _ := serviceFor(t, a, b)
	set := func(v string) {
		rel, err := s.Registry.ParseReleases([]byte("brief: {stable: \"" + v + "\"}"))
		if err != nil {
			t.Fatal(err)
		}
		s.Registry.SetReleases(rel)
	}
	set("1")
	entered := make(chan struct{})
	done := make(chan error, 1)
	first := true
	s.Model = providerFunc(func(ctx context.Context, _ *model.Request) (*model.Response, error) {
		if !first {
			return response("好"), nil
		}
		first = false
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _, err := s.Call(ctx, request()); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("model not entered")
	}
	set("2")
	if _, err := s.Call(context.Background(), request()); !errors.Is(err, model.ErrBusy) {
		t.Fatal("version bypassed declaration limit", err)
	}
	req := request()
	req.BusinessLine = "other"
	if got, err := s.Call(context.Background(), req); err != nil || got.Version != "2" {
		t.Fatalf("other business line lost its declaration slot: %+v, %v", got, err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not release call")
	}
}

func TestOutputModeratesDecodedJSON(t *testing.T) {
	s, uses := serviceFor(t, schemaDef())
	s.Moderator = moderation.Mock{Terms: []string{"<blocked>"}}
	calls := 0
	s.Model = providerFunc(func(context.Context, *model.Request) (*model.Response, error) {
		calls++
		return response(`{"brief":"\u003cblocked\u003e"}`), nil
	})
	result, err := s.Call(context.Background(), request())
	if !errors.Is(err, moderation.ErrRejected) || calls != 1 || result.Output != nil {
		t.Fatalf("unsafe output: %+v, %v", result, err)
	}
	if rows := records(t, uses, usage.ByOneshot); len(rows) != 1 || rows[0].Calls != 1 {
		t.Fatal(rows)
	}
}

func TestOutputModerationUnavailableDoesNotRepair(t *testing.T) {
	s, uses := serviceFor(t, schemaDef())
	calls := 0
	s.Model = providerFunc(func(context.Context, *model.Request) (*model.Response, error) {
		calls++
		return response(`{"brief":"好"}`), nil
	})
	s.Moderator = moderatorFunc(func(_ context.Context, req moderation.Request) (moderation.Verdict, error) {
		if req.Stage == moderation.Output {
			return moderation.Verdict{}, errors.New("private-output")
		}
		return moderation.Verdict{}, nil
	})
	result, err := s.Call(context.Background(), request())
	if !errors.Is(err, moderation.ErrUnavailable) || calls != 1 || result.Output != nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("%+v, %v", result, err)
	}
	if rows := records(t, uses, usage.ByOneshot); len(rows) != 1 || rows[0].Calls != 1 {
		t.Fatal(rows)
	}
}
