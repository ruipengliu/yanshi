package mcpserver

import (
	"context"
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
func TestCommitFailureHasNoAckOrNotification(t *testing.T) {
	x := newServer(nil)
	failure := errors.New("commit failed")
	notifier := &recordFiled{}
	runtime := newRuntime(failingStore{x.store, failure}, Options{Now: x.now, Notifier: notifier})
	for _, name := range []string{"file_changes", "file_day"} {
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
	if len(notifier.days) != 0 || len(st.Days) != 0 || len(st.Changesets) != 0 {
		t.Fatal("failed commit escaped")
	}
}

func TestRegistration(t *testing.T) {
	reg := XinliRegistration()
	if reg.Name != "xinli" || !reg.CiteCaptures || !reg.Acks {
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
