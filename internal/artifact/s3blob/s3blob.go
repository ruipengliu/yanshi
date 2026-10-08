// Package s3blob 是 artifact.BlobStore 的 S3 兼容实现（ADR-0011）。
package s3blob

import (
	"context"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"yanshi/internal/artifact"
)

type Config struct {
	Endpoint  string // host:port
	AccessKey string
	SecretKey string
	Bucket    string
	UseSSL    bool
	// Prefix 加在对象键前，便于多套环境共用一个桶。
	Prefix string
}

type Store struct {
	client *minio.Client
	bucket string
	prefix string
}

// New 连接对象存储并确保桶存在。
func New(ctx context.Context, cfg Config) (*Store, error) {
	c, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
	})
	if err != nil {
		return nil, err
	}
	exists, err := c.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return nil, err
	}
	if !exists {
		if err := c.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{}); err != nil {
			return nil, err
		}
	}
	return &Store{client: c, bucket: cfg.Bucket, prefix: cfg.Prefix}, nil
}

func (s *Store) Put(ctx context.Context, key string, r io.Reader) error {
	// 大小未知：以分片流式上传。
	_, err := s.client.PutObject(ctx, s.bucket, s.prefix+key, r, -1, minio.PutObjectOptions{PartSize: 16 << 20})
	return err
}

func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if _, err := s.client.StatObject(ctx, s.bucket, s.prefix+key, minio.StatObjectOptions{}); err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, artifact.ErrNotFound
		}
		return nil, err
	}
	return s.client.GetObject(ctx, s.bucket, s.prefix+key, minio.GetObjectOptions{})
}

func (s *Store) Delete(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, s.prefix+key, minio.RemoveObjectOptions{})
}
