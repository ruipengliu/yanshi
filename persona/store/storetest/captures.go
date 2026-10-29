package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"reflect"
	"strings"
	"testing"
	"time"

	"persona/domain"
	"persona/store"
)

const captureDate = "2026-10-27"

var captureNow = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

func seedCaptures(t *testing.T, s store.Store) {
	t.Helper()
	if err := s.Update(context.Background(), "bl", "u", func(b *domain.Book) error {
		if err := b.BindDay(captureDate, "day"); err != nil {
			return err
		}
		_, err := b.AppendCaptures(captureDate, "one", []domain.CaptureSegment{
			{Speaker: "wearer", Text: "原文甲", SpokenAt: captureNow.Add(-time.Hour).Format(time.RFC3339), TimeZone: "UTC"},
			{Speaker: "other", SpeakerKey: "other-1", SpeakerRef: "P-1", Text: "原文乙", SpokenAt: captureNow.Add(-59 * time.Minute).Format(time.RFC3339), TimeZone: "UTC"},
			{Speaker: "wearer", Text: "原文丙", SpokenAt: captureNow.Add(-58 * time.Minute).Format(time.RFC3339), TimeZone: "UTC"},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func captureOptions() domain.PageOptions {
	return domain.PageOptions{Limit: 200, MaxChars: 6000, GapMS: 300000, MaxToolResult: 8000}
}

func captures(t *testing.T, factory func() store.Store) {
	ctx := context.Background()
	t.Run("atomic_append", func(t *testing.T) {
		s := factory()
		seedCaptures(t, s)
		failure := errors.New("rollback")
		appendBoth := func(b *domain.Book) error {
			b.Seed(domain.SeedData{Projects: []domain.Project{{ID: "P-1", Name: "项目"}}}, captureNow)
			refs, err := b.AppendCaptures(captureDate, "two", []domain.CaptureSegment{{Speaker: "wearer", Text: "新增"}})
			if err == nil && !reflect.DeepEqual(refs, []string{"2.0"}) {
				return fmt.Errorf("allocation changed: %v", refs)
			}
			return err
		}
		if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
			if err := appendBoth(b); err != nil {
				return err
			}
			return failure
		}); !errors.Is(err, failure) {
			t.Fatal(err)
		}
		if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
			segs, err := b.DayCaptures(captureDate)
			if err != nil {
				return err
			}
			if len(segs) != 3 || len(b.State().Projects) != 0 {
				t.Fatal("partial append committed")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.Update(ctx, "bl", "u", appendBoth); err != nil {
			t.Fatal(err)
		}
		if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
			segs, err := b.DayCaptures(captureDate)
			if err != nil {
				return err
			}
			if len(segs) != 4 || len(b.State().Projects) != 1 {
				t.Fatal("missing atomic commit")
			}
			batch, err := b.CaptureAccess().Batch(captureDate, 1)
			if err != nil {
				return err
			}
			batch[0].Text = "改返回副本"
			// View 中允许在隔离副本演练领域命令，所有改动均不提交。
			_, err = b.EraseDay(captureDate, captureNow)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
			segs, err := b.DayCaptures(captureDate)
			if err != nil {
				return err
			}
			if len(segs) != 4 || segs[0].Text != "原文甲" || b.BusinessDay(captureDate).Deleted {
				t.Fatal("View leaked writes")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("snapshot", func(t *testing.T) {
		s := factory()
		seedCaptures(t, s)
		entered, release := make(chan struct{}), make(chan struct{})
		result := make(chan error, 1)
		go func() {
			result <- s.View(ctx, "bl", "u", func(b *domain.Book) error {
				// 先取得账本，等并发 Update 完成后才惰性读取原文。
				day := b.BusinessDay(captureDate)
				close(entered)
				<-release
				segs, err := b.DayCaptures(captureDate)
				if err != nil {
					return err
				}
				if day.Deleted || b.BusinessDay(captureDate).Deleted || len(segs) != 3 {
					return fmt.Errorf("mixed snapshots")
				}
				return nil
			})
		}()
		select {
		case <-entered:
		case err := <-result:
			t.Fatal("View failed before snapshot callback", err)
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("View did not enter snapshot callback")
		}
		update := make(chan error, 1)
		go func() {
			update <- s.Update(ctx, "bl", "u", func(b *domain.Book) error {
				_, err := b.EraseDay(captureDate, captureNow)
				return err
			})
		}()
		select {
		case err := <-update:
			close(release)
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("View blocked concurrent Update")
		}
		select {
		case err := <-result:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("View did not finish snapshot callback")
		}
		if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
			segs, err := b.DayCaptures(captureDate)
			if err != nil {
				return err
			}
			if !b.BusinessDay(captureDate).Deleted || len(segs) != 0 {
				t.Fatal("new snapshot missed erasure")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("lifetime", func(t *testing.T) {
		for _, mode := range []string{"update", "view", "rollback", "panic"} {
			t.Run(mode, func(t *testing.T) {
				s := factory()
				seedCaptures(t, s)
				var access domain.CaptureAccess
				var book *domain.Book
				var scan iter.Seq2[domain.CaptureRecord, error]
				var next func() (domain.CaptureRecord, error, bool)
				var stop func()
				failure := errors.New("rollback")
				fn := func(b *domain.Book) error {
					book, access = b, b.CaptureAccess()
					scan = access.Scan(captureDate, domain.Ref{}, domain.Ref{})
					next, stop = iter.Pull2(scan)
					for range 3 {
						if _, err, ok := next(); err != nil || !ok {
							t.Fatal("cannot suspend iterator at last segment", err)
						}
					}
					if mode == "rollback" {
						return failure
					}
					if mode == "panic" {
						panic(failure)
					}
					return nil
				}
				if mode == "panic" {
					func() {
						defer func() {
							if recover() != failure {
								t.Error("panic changed")
							}
						}()
						_ = s.Update(ctx, "bl", "u", fn)
					}()
				} else {
					transact := s.Update
					if mode == "view" {
						transact = s.View
					}
					err := transact(ctx, "bl", "u", fn)
					if mode == "rollback" {
						if !errors.Is(err, failure) {
							t.Fatal(err)
						}
					} else if err != nil {
						t.Fatal(err)
					}
				}
				check := func(err error) {
					t.Helper()
					if !errors.Is(err, domain.ErrCaptureTransactionClosed) {
						t.Fatalf("escaped access: %v", err)
					}
				}
				defer stop()
				_, err, ok := next()
				if !ok {
					t.Fatal("suspended iterator silently exhausted after transaction")
				}
				check(err)
				_, _, err = access.Lookup(captureDate, domain.Ref{Seq: 1})
				check(err)
				_, _, err = access.Adjacent(captureDate, domain.Ref{}, false)
				check(err)
				_, err = access.Batch(captureDate, 1)
				check(err)
				_, err = book.DayCaptures(captureDate)
				check(err)
				check(access.Append(captureDate, 2, []domain.CaptureSegment{{Speaker: "wearer", Text: "逃逸"}}))
				check(access.Erase(captureDate, domain.Ref{}, domain.Ref{}))
				for _, sequence := range []iter.Seq2[domain.CaptureRecord, error]{scan, access.Scan(captureDate, domain.Ref{}, domain.Ref{})} {
					count := 0
					for _, err := range sequence {
						count++
						check(err)
					}
					if count != 1 {
						t.Fatal("escaped iterator hid error")
					}
				}
			})
		}
	})
	t.Run("ledger_only_snapshot", func(t *testing.T) {
		s := factory()
		seedCaptures(t, s)
		if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
			clone, err := b.Clone()
			if err != nil {
				return err
			}
			raw, err := json.Marshal(b)
			if err != nil {
				return err
			}
			if strings.Contains(string(raw), "原文") || strings.Contains(string(raw), "SpeakerKey") || clone.CaptureAccess() != nil {
				t.Fatal("snapshot contains capture payload/access")
			}
			_, err = clone.DayCaptures(captureDate)
			if !errors.Is(err, domain.ErrCaptureTransactionClosed) {
				t.Fatal("clone silently read missing captures", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("cancellation", func(t *testing.T) { captureCancellation(t, factory) })
	t.Run("reads", func(t *testing.T) {
		captureReads(t, factory)
	})
	t.Run("ranges", func(t *testing.T) {
		captureAccessRanges(t, factory)
	})
}

// 包装接缝只注入基础读取错误，不在测试中复刻领域判断。
type captureReadFault struct {
	domain.CaptureAccess
	operation           string
	failure             error
	scanCalls, yielded  int
	failScan, failAfter int
}

func (f *captureReadFault) Lookup(date string, ref domain.Ref) (domain.CaptureSegment, bool, error) {
	if f.operation == "lookup" {
		return domain.CaptureSegment{}, false, f.failure
	}
	return f.CaptureAccess.Lookup(date, ref)
}

func (f *captureReadFault) Batch(date string, number uint64) ([]domain.CaptureSegment, error) {
	if f.operation == "batch" {
		return nil, f.failure
	}
	return f.CaptureAccess.Batch(date, number)
}

func (f *captureReadFault) Adjacent(date string, ref domain.Ref, before bool) (domain.CaptureRecord, bool, error) {
	if f.operation == "adjacent" {
		return domain.CaptureRecord{}, false, f.failure
	}
	return f.CaptureAccess.Adjacent(date, ref, before)
}

func (f *captureReadFault) Scan(date string, from, to domain.Ref) iter.Seq2[domain.CaptureRecord, error] {
	return func(yield func(domain.CaptureRecord, error) bool) {
		f.scanCalls++
		count := 0
		for r, err := range f.CaptureAccess.Scan(date, from, to) {
			f.yielded++
			count++
			if f.operation == "scan" && (f.failScan == 0 || f.failScan == f.scanCalls) && (f.failAfter == 0 || count >= f.failAfter) {
				yield(domain.CaptureRecord{}, f.failure)
				return
			}
			if !yield(r, err) || err != nil {
				return
			}
		}
	}
}

func captureReads(t *testing.T, factory func() store.Store) {
	ctx := context.Background()
	for _, operation := range []string{"lookup", "batch", "page", "tail", "progress", "erase_scan", "erase_rewind", "adjacent", "day"} {
		t.Run(operation, func(t *testing.T) {
			s := factory()
			seedCaptures(t, s)
			if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
				_, err := domain.NewCall(b, "day", nil, nil, captureNow).Captures(captureOptions())
				return err
			}); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("capture read failed")
			err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
				fault := &captureReadFault{CaptureAccess: b.CaptureAccess(), operation: operation, failure: failure}
				b.SetCaptureAccess(fault)
				// 连同读失败之前的账本修改也必须回滚。
				b.Seed(domain.SeedData{Projects: []domain.Project{{ID: "P-fail"}}}, captureNow)
				switch operation {
				case "batch":
					_, err := b.AppendCaptures(captureDate, "one", []domain.CaptureSegment{{Speaker: "wearer", Text: "有效参数"}})
					return err
				case "page", "tail":
					fault.operation = "scan"
					fault.failAfter = 2
					if operation == "tail" {
						fault.failScan = 2
					}
					_, err := domain.NewCall(b, "day", json.RawMessage(`{"limit":1}`), nil, captureNow).Captures(captureOptions())
					return err
				case "progress", "lookup":
					if operation == "progress" {
						fault.operation = "scan"
						fault.failAfter = 2
					}
					c := domain.NewCall(b, "day", json.RawMessage(`{"source":{"from":"#1.0","to":"#1.2"}}`), nil, captureNow)
					text := c.FileChanges()
					if text != "" {
						t.Fatal("read error became domain receipt", text)
					}
					return c.Err()
				case "erase_scan", "erase_rewind":
					fault.operation = "scan"
					if operation == "erase_rewind" {
						fault.failScan = 2
					}
					_, err := b.Erase(captureDate, "1.1", "1.1", captureNow)
					return err
				case "adjacent":
					_, err := b.Erase(captureDate, "1.1", "1.1", captureNow)
					return err
				default:
					fault.operation = "scan"
					_, err := b.DayCaptures(captureDate)
					return err
				}
			})
			if !errors.Is(err, failure) {
				t.Fatalf("lost read error: %v", err)
			}
			if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
				segs, err := b.DayCaptures(captureDate)
				if err != nil {
					return err
				}
				if len(segs) != 3 || segs[1].Text != "原文乙" || len(b.State().Projects) != 0 || len(b.State().Changesets) != 0 || len(b.DeletionTasks()) != 0 || b.CaptureProgress(captureDate) != "0" {
					t.Fatal("read failure partially committed")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("view_error", func(t *testing.T) {
		s := factory()
		seedCaptures(t, s)
		failure := errors.New("view capture read failed")
		err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
			b.SetCaptureAccess(&captureReadFault{CaptureAccess: b.CaptureAccess(), operation: "scan", failure: failure})
			_, err := b.DayCaptures(captureDate)
			return err
		})
		if !errors.Is(err, failure) {
			t.Fatal("View hid read error", err)
		}
	})
	t.Run("lazy_page", func(t *testing.T) {
		s := factory()
		seedCaptures(t, s)
		if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
			probe := &captureReadFault{CaptureAccess: b.CaptureAccess()}
			b.SetCaptureAccess(probe)
			if _, err := b.Clone(); err != nil {
				return err
			}
			_ = b.BusinessDay(captureDate)
			if probe.yielded != 0 {
				t.Fatal("ledger loaded captures")
			}
			p, err := domain.NewCall(b, "day", json.RawMessage(`{"limit":1,"all":true}`), nil, captureNow).Captures(captureOptions())
			if err != nil {
				return err
			}
			if len(p.Segments) != 1 || probe.yielded != 2 {
				t.Fatal("page eagerly consumed whole day")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
}

func captureAccessRanges(t *testing.T, factory func() store.Store) {
	ctx := context.Background()
	s := factory()
	seedCaptures(t, s)
	if err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		if _, err := b.AppendCaptures(captureDate, "two", []domain.CaptureSegment{{Speaker: "wearer", Text: "第二批"}}); err != nil {
			return err
		}
		if _, err := b.AppendCaptures("2026-10-28", "one", []domain.CaptureSegment{{Speaker: "wearer", Text: "另一天"}}); err != nil {
			return err
		}
		_, err := b.Erase(captureDate, "1.1", "1.1", captureNow)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.View(ctx, "bl", "u", func(b *domain.Book) error {
		a := b.CaptureAccess()
		for _, ref := range []domain.Ref{{Seq: 1, Index: 1}, {Seq: 99}, {Seq: 1, Index: -1}} {
			_, found, err := a.Lookup(captureDate, ref)
			if err != nil {
				return err
			}
			if found {
				t.Fatal("missing or erased ref exists")
			}
		}
		batch, err := a.Batch(captureDate, 1)
		if err != nil {
			return err
		}
		if len(batch) != 3 || batch[1] != (domain.CaptureSegment{}) {
			t.Fatal("erasure lost numbering or retained metadata")
		}
		var refs []domain.Ref
		for r, err := range a.Scan(captureDate, domain.Ref{Seq: 1, Index: 1}, domain.Ref{Seq: 2}) {
			if err != nil {
				return err
			}
			refs = append(refs, r.Ref)
		}
		if !reflect.DeepEqual(refs, []domain.Ref{{Seq: 1, Index: 2}, {Seq: 2}}) {
			t.Fatal("range order/bounds", refs)
		}
		for _, tc := range []struct {
			ref    domain.Ref
			before bool
			want   domain.Ref
		}{
			{domain.Ref{}, false, domain.Ref{Seq: 1}},
			{domain.Ref{}, true, domain.Ref{Seq: 2}},
			{domain.Ref{Seq: 1}, false, domain.Ref{Seq: 1, Index: 2}},
			{domain.Ref{Seq: 2}, true, domain.Ref{Seq: 1, Index: 2}},
		} {
			r, found, err := a.Adjacent(captureDate, tc.ref, tc.before)
			if err != nil {
				return err
			}
			if !found || r.Ref != tc.want {
				t.Fatal("adjacent", r.Ref, found)
			}
		}
		g, found, err := a.Lookup("2026-10-28", domain.Ref{Seq: 1})
		if err != nil {
			return err
		}
		if !found || g.Text != "另一天" {
			t.Fatal("cross-day erasure")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []store.Owner{{BusinessLine: "other", EndUser: "u"}, {BusinessLine: "bl", EndUser: "other"}} {
		if err := s.View(ctx, owner.BusinessLine, owner.EndUser, func(b *domain.Book) error {
			_, found, err := b.CaptureAccess().Lookup(captureDate, domain.Ref{Seq: 1})
			if err != nil {
				return err
			}
			if found {
				t.Fatal("cross-owner lookup")
			}
			for _, err := range b.CaptureAccess().Scan(captureDate, domain.Ref{}, domain.Ref{}) {
				if err != nil {
					return err
				}
				t.Fatal("cross-owner scan")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func captureCancellation(t *testing.T, factory func() store.Store) {
	s := factory()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := s.Update(ctx, "bl", "u", func(b *domain.Book) error {
		if _, err := b.AppendCaptures(captureDate, "one", []domain.CaptureSegment{{Speaker: "wearer", Text: "原文"}}); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := s.Update(context.Background(), "bl", "u", func(b *domain.Book) error {
		segs, err := b.DayCaptures(captureDate)
		if err != nil {
			return err
		}
		if len(segs) != 0 {
			t.Fatal("cancelled append committed")
		}
		refs, err := b.AppendCaptures(captureDate, "one", []domain.CaptureSegment{{Speaker: "wearer", Text: "重试"}})
		if err == nil && !reflect.DeepEqual(refs, []string{"1.0"}) {
			t.Fatal("cancelled allocation committed")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
