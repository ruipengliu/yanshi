// Package artifacttest 是 artifact.BlobStore 与 artifact.MetaStore 实现的一致性测试套件。
package artifacttest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"yanshi/internal/artifact"
)

func RunBlob(t *testing.T, newStore func(t *testing.T) artifact.BlobStore) {
	ctx := context.Background()
	t.Run("PutGetDelete", func(t *testing.T) {
		s := newStore(t)
		data := bytes.Repeat([]byte("yanshi"), 100000) // 600KB，跨越缓冲区
		if err := s.Put(ctx, "k1", bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
		rc, err := s.Get(ctx, "k1")
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(rc)
		rc.Close()
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("read back %d bytes, err %v", len(got), err)
		}
		if err := s.Delete(ctx, "k1"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Get(ctx, "k1"); !errors.Is(err, artifact.ErrNotFound) {
			t.Fatalf("get after delete err = %v", err)
		}
		if err := s.Delete(ctx, "k1"); err != nil {
			t.Fatalf("delete missing err = %v", err)
		}
	})
	t.Run("MissingIsNotFound", func(t *testing.T) {
		if _, err := newStore(t).Get(ctx, "nope"); !errors.Is(err, artifact.ErrNotFound) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("FailedPutLeavesNothing", func(t *testing.T) {
		s := newStore(t)
		err := s.Put(ctx, "k2", io.MultiReader(strings.NewReader("partial"), errReader{}))
		if err == nil {
			t.Fatal("put with failing reader succeeded")
		}
		if rc, err := s.Get(ctx, "k2"); err == nil {
			b, _ := io.ReadAll(rc)
			rc.Close()
			t.Fatalf("partial object visible: %q", b)
		}
	})
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("boom") }

func RunMeta(t *testing.T, newStore func(t *testing.T) artifact.MetaStore) {
	ctx := context.Background()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t.Run("CreateGetList", func(t *testing.T) {
		s := newStore(t)
		for i, id := range []string{"b", "a", "c"} {
			sid := "s1"
			if id == "c" {
				sid = "s2"
			}
			if err := s.Create(ctx, &artifact.Meta{ID: id, SessionID: sid, Name: id + ".txt", MimeType: "text/plain",
				Size: int64(i), SHA256: "h" + id, CreatedAt: t0.Add(time.Duration(i) * time.Second)}); err != nil {
				t.Fatal(err)
			}
		}
		m, err := s.Get(ctx, "a")
		if err != nil || m.Name != "a.txt" || m.Size != 1 || m.SHA256 != "ha" || !m.CreatedAt.Equal(t0.Add(time.Second)) {
			t.Fatalf("get = %+v, %v", m, err)
		}
		list, err := s.List(ctx, "s1")
		if err != nil || len(list) != 2 || list[0].ID != "b" || list[1].ID != "a" {
			t.Fatalf("list = %+v, %v", list, err)
		}
		if _, err := s.Get(ctx, "zz"); !errors.Is(err, artifact.ErrNotFound) {
			t.Fatalf("missing err = %v", err)
		}
		if err := s.Delete(ctx, "a"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Get(ctx, "a"); !errors.Is(err, artifact.ErrNotFound) {
			t.Fatal("deleted meta still present")
		}
		if list, _ := s.List(ctx, "s1"); len(list) != 1 {
			t.Fatalf("list after delete = %d", len(list))
		}
		if err := s.Delete(ctx, "a"); err != nil {
			t.Fatalf("delete is not idempotent: %v", err)
		}
	})
}
