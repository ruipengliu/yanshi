package artifact_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"yanshi/internal/artifact"
	"yanshi/internal/artifact/artifacttest"
	"yanshi/internal/artifact/fsblob"
	"yanshi/internal/artifact/pgartifact"
	"yanshi/internal/artifact/s3blob"
	"yanshi/internal/clock"
	"yanshi/internal/ids"
	"yanshi/internal/lifecycle"
	"yanshi/internal/pg/pgtest"
)

func TestMemMeta(t *testing.T) {
	artifacttest.RunMeta(t, func(*testing.T) artifact.MetaStore { return artifact.NewMemMeta() })
}

func TestPGMeta(t *testing.T) {
	artifacttest.RunMeta(t, func(t *testing.T) artifact.MetaStore {
		pool, _ := pgtest.Fresh(t)
		return pgartifact.Meta{Pool: pool}
	})
}

func TestMemBlobs(t *testing.T) {
	artifacttest.RunBlob(t, func(*testing.T) artifact.BlobStore { return artifact.NewMemBlobs() })
}

func TestFSBlob(t *testing.T) {
	artifacttest.RunBlob(t, func(t *testing.T) artifact.BlobStore { return fsblob.Store{Dir: t.TempDir()} })
}

// S3Config 返回测试用对象存储配置；未设置 YANSHI_TEST_S3 时跳过。
func s3Store(t *testing.T) artifact.BlobStore {
	endpoint := os.Getenv("YANSHI_TEST_S3")
	if endpoint == "" {
		t.Skip("YANSHI_TEST_S3 not set")
	}
	s, err := s3blob.New(context.Background(), s3blob.Config{
		Endpoint: endpoint, AccessKey: "yanshi", SecretKey: "yanshi-dev-secret", Bucket: "yanshi-test",
		Prefix: t.Name() + "/" + time.Now().Format("150405.000000") + "/",
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestS3Blob(t *testing.T) {
	artifacttest.RunBlob(t, s3Store)
}

func service(t *testing.T, max int64) *artifact.Service {
	return &artifact.Service{Meta: artifact.NewMemMeta(), Blobs: fsblob.Store{Dir: t.TempDir()},
		IDs: ids.Sequential("x"), Clock: clock.Real{}, MaxSize: max}
}

func TestServicePutOpen(t *testing.T) {
	s := service(t, 0)
	data := []byte("周一例会纪要")
	m, err := s.Put(context.Background(), "ses_v1_000_000_ses1", "../../notes.txt", "", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if m.Name != "notes.txt" || m.MimeType != "text/plain; charset=utf-8" || m.Size != int64(len(data)) || m.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("meta = %+v", m)
	}
	got, rc, err := s.Open(context.Background(), m.SessionID, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(b, data) || got.ID != m.ID {
		t.Fatalf("open = %q", b)
	}
	if !strings.Contains(artifact.Describe(m.Block().GetMedia()), m.ID+": notes.txt") {
		t.Fatalf("describe = %s", artifact.Describe(m.Block().GetMedia()))
	}
}

func TestServiceRejectsOversize(t *testing.T) {
	s := service(t, 10)
	_, err := s.Put(context.Background(), "ses_v1_000_000_ses1", "big.bin", "", bytes.NewReader(make([]byte, 11)))
	if !errors.Is(err, artifact.ErrTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if list, _ := s.List(context.Background(), "ses_v1_000_000_ses1"); len(list) != 0 {
		t.Fatalf("oversize artifact recorded: %+v", list)
	}
	if _, err := s.Put(context.Background(), "ses_v1_000_000_ses1", "ok.bin", "", bytes.NewReader(make([]byte, 10))); err != nil {
		t.Fatalf("exactly-at-limit err = %v", err)
	}
}

func TestServiceArtifactSlot(t *testing.T) {
	ctx := context.Background()
	sid, err := ids.Sequential("session").Session("u")
	if err != nil {
		t.Fatal(err)
	}
	for _, sessionID := range []string{sid} {
		s := service(t, 0)
		want, err := ids.SessionSlotOf(sessionID)
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			m, err := s.Put(ctx, sessionID, "note.txt", "text/plain", strings.NewReader("data"))
			if err != nil {
				t.Fatal(err)
			}
			slot, err := ids.SlotOf(m.ID)
			if err != nil || slot != want || m.SessionID != sessionID {
				t.Fatalf("artifact %s: %d, %v; want %d", m.ID, slot, err, want)
			}
			id, ok := artifact.ParseURI(artifact.URI(m.ID))
			if !ok || id != m.ID {
				t.Fatalf("URI round trip: %s", m.ID)
			}
			_, rc, err := s.Open(ctx, sessionID, id)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(rc)
			rc.Close()
			if err != nil || string(data) != "data" {
				t.Fatalf("blob round trip: %q, %v", data, err)
			}
		}
	}
	s := service(t, 0)
	if _, err := s.Put(ctx, "ses_v1_100_000_x", "note.txt", "text/plain", strings.NewReader("data")); err == nil {
		t.Fatal("malformed Session accepted")
	}
	if rows, err := s.List(ctx, "ses_v1_100_000_x"); err != nil || len(rows) != 0 {
		t.Fatalf("malformed Session left metadata: %v, %v", rows, err)
	}
}

// 委托交接的复制（ADR-0055）：副本属于目标 Session、内容相同，重复复制返回同一份；来源必须属于声称的 Session；
// 目标 Session 已删除时撤回副本。
func TestServiceCopy(t *testing.T) {
	ctx := context.Background()
	s := service(t, 0)
	dels := lifecycle.NewMemDeletions()
	s.Deletions = dels
	const parent, child, other = "ses_v1_000_000_p", "ses_v1_001_000_c", "ses_v1_002_000_o"
	src, err := s.Put(ctx, parent, "report.pdf", "application/pdf", strings.NewReader("pdf bytes"))
	if err != nil {
		t.Fatal(err)
	}
	const dst = "art_v1_001_copy"
	m, err := s.Copy(ctx, parent, src.ID, child, dst)
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != dst || m.SessionID != child || m.Name != src.Name || m.MimeType != src.MimeType || m.Size != src.Size || m.SHA256 != src.SHA256 {
		t.Fatalf("copy meta = %+v, source %+v", m, src)
	}
	_, rc, err := s.Open(ctx, child, dst)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "pdf bytes" {
		t.Fatalf("copy content = %q", b)
	}
	if again, err := s.Copy(ctx, parent, src.ID, child, dst); err != nil || again.ID != dst || !again.CreatedAt.Equal(m.CreatedAt) {
		t.Fatalf("repeated copy = %+v, %v", again, err)
	}
	// 来源须属于声称的 Session：别的 Session 的工件与不存在的一样。
	if _, err := s.Copy(ctx, other, src.ID, child, "art_v1_001_copy2"); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("foreign source err = %v", err)
	}
	// 删除来源 Session 的工件不影响副本。
	if err := s.DeleteSession(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stat(ctx, child, dst); err != nil {
		t.Fatalf("copy gone with source: %v", err)
	}
	// 目标 Session 已有删除记录：先写后查撤回副本。
	src2, err := s.Put(ctx, other, "a.txt", "text/plain", strings.NewReader("a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dels.Mark(ctx, &lifecycle.Tombstone{SessionID: child, Reason: "end_user", RequestedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Copy(ctx, other, src2.ID, child, "art_v1_001_copy3"); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("copy into deleted session err = %v", err)
	}
	if _, err := s.Stat(ctx, child, "art_v1_001_copy3"); !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("copy into deleted session left metadata: %v", err)
	}
}
