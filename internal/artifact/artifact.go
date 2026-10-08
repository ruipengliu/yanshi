// Package artifact 存储 Session 的工件：元数据（MetaStore）与字节内容（BlobStore）分离。
// 语义见 docs/design/m2-artifacts.md，存储契约见 ADR-0011。
package artifact

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	v1 "yanshi/gen/yanshi/v1"
	"yanshi/internal/clock"
	"yanshi/internal/ids"
	"yanshi/internal/model"
)

var (
	ErrNotFound = errors.New("artifact: not found")
	ErrTooLarge = errors.New("artifact: too large")
)

// Scheme 是 Media.uri 中引用工件的前缀。
const Scheme = "artifact://"

func URI(id string) string { return Scheme + id }

// ParseURI 从 "artifact://<id>" 中取出 ID。
func ParseURI(uri string) (string, bool) {
	id, ok := strings.CutPrefix(uri, Scheme)
	return id, ok && id != ""
}

type Meta struct {
	ID        string
	SessionID string
	Name      string
	MimeType  string
	Size      int64
	SHA256    string
	CreatedAt time.Time
}

// Block 返回引用该工件的内容块。
func (m *Meta) Block() *v1.ContentBlock {
	return &v1.ContentBlock{Kind: &v1.ContentBlock_Media{Media: &v1.Media{
		Uri: URI(m.ID), MimeType: m.MimeType, Name: m.Name, Size: uint64(m.Size),
	}}}
}

// Describe 把 Media 呈现为给模型看的一行文本。
func Describe(m *v1.Media) string { return model.DescribeMedia(m) }

func HumanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

type MetaStore interface {
	Create(ctx context.Context, m *Meta) error
	Get(ctx context.Context, id string) (*Meta, error)
	// List 返回 Session 的全部工件，按创建时间排序。
	List(ctx context.Context, sessionID string) ([]*Meta, error)
}

type BlobStore interface {
	Put(ctx context.Context, key string, r io.Reader) error
	// Get 返回内容；不存在时返回 ErrNotFound。
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
}

// DefaultMaxSize 是单个工件的大小上限。
const DefaultMaxSize = 512 << 20

type Service struct {
	Meta    MetaStore
	Blobs   BlobStore
	IDs     ids.Generator
	Clock   clock.Clock
	MaxSize int64
}

// limitReader 在超过上限时返回 ErrTooLarge，而不是静默截断。
type limitReader struct {
	r    io.Reader
	left int64
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.left <= 0 {
		var one [1]byte
		if n, _ := l.r.Read(one[:]); n > 0 {
			return 0, ErrTooLarge
		}
		return 0, io.EOF
	}
	if int64(len(p)) > l.left {
		p = p[:l.left]
	}
	n, err := l.r.Read(p)
	l.left -= int64(n)
	return n, err
}

// Put 写入一个新工件。mimeType 为空时按文件名扩展名、再按内容推断。
func (s *Service) Put(ctx context.Context, sessionID, name, mimeType string, r io.Reader) (*Meta, error) {
	if sessionID == "" {
		return nil, errors.New("artifact: session is required")
	}
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	if name == "." || name == "/" {
		name = "file"
	}
	br := bufio.NewReader(r)
	if mimeType == "" {
		mimeType = mime.TypeByExtension(path.Ext(name))
	}
	if mimeType == "" {
		head, _ := br.Peek(512)
		mimeType = http.DetectContentType(head)
	}
	max := s.MaxSize
	if max == 0 {
		max = DefaultMaxSize
	}
	m := &Meta{ID: "art_" + s.IDs(), SessionID: sessionID, Name: name, MimeType: mimeType, CreatedAt: s.Clock.Now()}
	h := sha256.New()
	counter := &countWriter{}
	body := io.TeeReader(&limitReader{r: br, left: max}, io.MultiWriter(h, counter))
	if err := s.Blobs.Put(ctx, m.ID, body); err != nil {
		_ = s.Blobs.Delete(context.WithoutCancel(ctx), m.ID)
		if errors.Is(err, ErrTooLarge) {
			return nil, fmt.Errorf("%w: limit %s", ErrTooLarge, HumanSize(max))
		}
		return nil, err
	}
	m.Size, m.SHA256 = counter.n, hex.EncodeToString(h.Sum(nil))
	if err := s.Meta.Create(ctx, m); err != nil {
		_ = s.Blobs.Delete(context.WithoutCancel(ctx), m.ID)
		return nil, err
	}
	return m, nil
}

type countWriter struct{ n int64 }

func (c *countWriter) Write(p []byte) (int, error) { c.n += int64(len(p)); return len(p), nil }

func (s *Service) Stat(ctx context.Context, id string) (*Meta, error) { return s.Meta.Get(ctx, id) }

func (s *Service) Open(ctx context.Context, id string) (*Meta, io.ReadCloser, error) {
	m, err := s.Meta.Get(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	rc, err := s.Blobs.Get(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	return m, rc, nil
}

func (s *Service) List(ctx context.Context, sessionID string) ([]*Meta, error) {
	return s.Meta.List(ctx, sessionID)
}
