package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"persona/domain"
	"persona/store"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type notifierFunc func(context.Context, string, string, string, string, []string)

func (f notifierFunc) DayFiled(ctx context.Context, bl, eu, sid, date string, projects []string) {
	f(ctx, bl, eu, sid, date, projects)
}

func TestNotificationAfterCommit(t *testing.T) {
	x := newServer(nil)
	notified := 0
	x.notifier = notifierFunc(func(ctx context.Context, bl, eu, sid, date string, projects []string) {
		notified++
		// 重入同键读取并更新绑定：若仍持存储锁，此处会死锁；若未提交，读不到 Day。
		if err := x.store.Update(ctx, bl, eu, func(b *domain.Book) error {
			days := b.State().Days
			if len(days) != 1 || days[0].Session != sid || days[0].Date != date {
				t.Fatalf("notification before commit: %+v", days)
			}
			b.BindWeek("2026-10-17", "notified")
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if len(projects) != 0 {
			t.Fatal(projects)
		}
	})
	invoke(x, "day", "file_day", M{"day": M{"headline": "今日"}})
	if notified != 1 || x.ReviewWeek("bl", "u").Session != "notified" {
		t.Fatal("missing notification")
	}
	invoke(x, "day", "file_day", M{"day": M{"headline": "失眠"}})
	if notified != 1 {
		t.Fatal("rejection notified")
	}
	if changes, err := x.store.PendingDomainChanges(context.Background(), "bl", "u"); err != nil || len(changes) != 1 {
		t.Fatal("rejection changed accepted facts", changes, err)
	}
}

type failingStore struct {
	store.Store
	err error
}

func (s failingStore) Update(ctx context.Context, bl, eu string, fn func(*domain.Book) error) error {
	return s.Store.Update(ctx, bl, eu, func(b *domain.Book) error {
		if err := fn(b); err != nil {
			return err
		}
		return s.err
	})
}
func TestCommitFailureHasNoResultOrNotification(t *testing.T) {
	x := newServer(nil)
	failure := errors.New("commit failed")
	notifier := &recordFiled{}
	runtime := newRuntime(failingStore{x.store, failure}, Options{AllowUnmanaged: true, Now: x.now, Notifier: notifier})
	for _, name := range []string{"file_changes", "file_day", "file_week"} {
		for _, tool := range tools {
			if tool.tool.Name != name {
				continue
			}
			result, err := runtime.call(context.Background(), mcp.Meta{"yanshi/business_line": "bl", "yanshi/end_user": "u", "yanshi/session": "day"},
				[]byte(`{"source":{"from":"#2.0","to":"#2.1"},"day":{"headline":"今日"}}`), tool.handler, false)
			if result != nil || !errors.Is(err, failure) {
				t.Fatalf("result=%+v error=%v", result, err)
			}
		}
	}
	st := x.State("bl", "u")
	if len(notifier.days) != 0 || len(st.Days) != 0 || len(st.Changesets) != 0 || len(st.Weeks) != 0 {
		t.Fatal("failed commit escaped")
	}
	if changes, err := x.store.PendingDomainChanges(context.Background(), "bl", "u"); err != nil || len(changes) != 0 {
		t.Fatal("failed commit emitted domain change", changes, err)
	}
}

func TestRegistration(t *testing.T) {
	reg := XinliRegistration()
	if reg.Name != "xinli" || !reg.CiteCaptures || reg.Acks || !reg.AttestsSpeakers {
		t.Fatalf("registration = %+v", reg)
	}
	for _, name := range reg.NoApproval {
		found := false
		for _, tool := range tools {
			if tool.tool.Name == name {
				found = true
				if tool.tool.Annotations != nil && tool.tool.Annotations.ReadOnlyHint {
					t.Fatal(name)
				}
			}
		}
		if !found {
			t.Fatal(name)
		}
	}
	reg.NoApproval[0] = "changed"
	if XinliRegistration().NoApproval[0] == "changed" {
		t.Fatal("registration shares mutable data")
	}
}

// 已停用的工作 Session 不能借未绑定兼容路径提交，拒收也不能通知归集。
func TestInactiveDaySessionCannotFile(t *testing.T) {
	x := newServer(nil)
	notifier := &recordFiled{}
	x.notifier = notifier
	if err := x.store.Update(context.Background(), "bl", "u", func(b *domain.Book) error {
		if err := b.BindDay("2026-10-23", "old"); err != nil {
			return err
		}
		_, err := b.EraseDay("2026-10-23", x.now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"file_changes", "file_day"} {
		got := invoke(x, "old", name, M{"source": M{"day": true}, "day": M{"headline": "迟到的总览"}})
		if got != domain.ErrInactiveSession.Error() {
			t.Fatal(name, got)
		}
	}
	st := x.State("bl", "u")
	if len(st.Changesets) != 0 || len(st.Days) != 0 || len(notifier.days) != 0 {
		t.Fatal("inactive session mutated knowledge or notified")
	}
}

// 原文读取故障须沿普通工具的错误通道回滚，不能被翻译为“引用不存在”的拒收文字。
type brokenCaptureLookup struct {
	domain.CaptureAccess
	failure error
}

func (b brokenCaptureLookup) Lookup(string, domain.Ref) (domain.CaptureSegment, bool, error) {
	return domain.CaptureSegment{}, false, b.failure
}

type brokenCaptureStore struct {
	store.Store
	failure error
}

func (s brokenCaptureStore) Update(ctx context.Context, bl, eu string, fn func(*domain.Book) error) error {
	return s.Store.Update(ctx, bl, eu, func(b *domain.Book) error {
		b.SetCaptureAccess(brokenCaptureLookup{b.CaptureAccess(), s.failure})
		return fn(b)
	})
}
func TestCaptureReadFailureHasNoReceipt(t *testing.T) {
	x := newServer(nil)
	ctx := context.Background()
	if err := x.store.Update(ctx, "bl", "u", func(b *domain.Book) error {
		if err := b.BindDay("2026-10-23", "day"); err != nil {
			return err
		}
		_, err := b.AppendCaptures("2026-10-23", "one", []domain.CaptureSegment{{Speaker: "wearer", Text: "原文"}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("capture read failed")
	runtime := newRuntime(brokenCaptureStore{x.store, failure}, Options{AllowUnmanaged: true, Now: x.now})
	result, err := runtime.call(ctx, mcp.Meta{"yanshi/business_line": "bl", "yanshi/end_user": "u", "yanshi/session": "day"},
		[]byte(`{"source":{"from":"#1.0","to":"#1.0"}}`), (*domain.Call).FileChanges, false)
	if result != nil || !errors.Is(err, failure) {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if len(x.State("bl", "u").Changesets) != 0 {
		t.Fatal("read failure committed")
	}
}

func TestProjectCaptureReadFailureHasNoReceipt(t *testing.T) {
	x := newServer(nil)
	ctx := context.Background()
	var revision int
	if err := x.store.Update(ctx, "bl", "u", func(b *domain.Book) error {
		if err := b.BindDay("2026-10-23", "day"); err != nil {
			return err
		}
		b.Seed(domain.SeedData{
			Projects:    []domain.Project{{ID: "P-1", Name: "项目"}},
			Commitments: []domain.SeedCommitment{{ID: "C-0001", Kind: "mine", Content: "发报价", Project: "P-1", Status: "open"}},
		}, x.now())
		b.Bind("P-1", "project")
		revision = b.State().Commitments[0].Rev()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("capture read failed")
	runtime := newRuntime(brokenCaptureStore{x.store, failure}, Options{AllowUnmanaged: true, Now: x.now})
	args, err := json.Marshal(M{"date": "2026-10-23", "close": []M{{"id": "C-0001", "rev": revision, "status": "done", "refs": []string{"#1.0"}, "exchanges": []string{"x1.0"}}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.call(ctx, mcp.Meta{"yanshi/business_line": "bl", "yanshi/end_user": "u", "yanshi/session": "project"}, args, (*domain.Call).UpdateCommitments, false)
	if result != nil || !errors.Is(err, failure) {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if st := x.State("bl", "u"); len(st.Changesets) != 1 || st.Commitments[0].Status != "open" {
		t.Fatal("project read failure committed")
	}
}
