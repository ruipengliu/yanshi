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
	m, err := s.Put(context.Background(), "ses1", "../../notes.txt", "", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if m.Name != "notes.txt" || m.MimeType != "text/plain; charset=utf-8" || m.Size != int64(len(data)) || m.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("meta = %+v", m)
	}
	got, rc, err := s.Open(context.Background(), m.ID)
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
	_, err := s.Put(context.Background(), "ses1", "big.bin", "", bytes.NewReader(make([]byte, 11)))
	if !errors.Is(err, artifact.ErrTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if list, _ := s.List(context.Background(), "ses1"); len(list) != 0 {
		t.Fatalf("oversize artifact recorded: %+v", list)
	}
	if _, err := s.Put(context.Background(), "ses1", "ok.bin", "", bytes.NewReader(make([]byte, 10))); err != nil {
		t.Fatalf("exactly-at-limit err = %v", err)
	}
}
