package query_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"persona/domain"
	"persona/query"
	"persona/store"
	"persona/store/memstore"
)

func TestCaptureViewQueryMetadata(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC) }
	books := memstore.New(now)
	segments := []domain.CaptureSegment{
		{Speaker: "unknown", Text: "未知", Background: true},
		{Speaker: "wearer", SpeakerGrade: "timeline", Text: "隔离", Disposition: "quarantine", DispositionReason: "playback", LowSupportSpans: []domain.LowSupportSpan{{Start: 0, End: 1, CriticalKinds: []string{"person"}}}},
	}
	if err := books.Update(context.Background(), "bl", "u", func(b *domain.Book) error {
		_, err := b.AppendCaptures("2026-10-28", "one", segments)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	svc := query.Service{Store: books, Now: now}
	cursor := ""
	for i, want := range segments {
		raw, err := svc.Read(context.Background(), store.Owner{BusinessLine: "bl", EndUser: "u"}, query.Request{Resource: "captures", Date: "2026-10-28", Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		var page struct {
			Items           []domain.CaptureView `json:"items"`
			NextCursor      string               `json:"next_cursor"`
			CaptureRevision uint64               `json:"capture_revision"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 || !page.Items[0].CaptureSegment.Equal(want) || page.CaptureRevision != 1 || (page.NextCursor != "") != (i == 0) {
			t.Fatalf("query metadata changed: %s", raw)
		}
		cursor = page.NextCursor
	}
}
