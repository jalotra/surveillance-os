// Package blob is the R2 object store. Everything external goes through
// the Client interface so the chunker and API stay testable without
// credentials, and a Discard client keeps local dev functional.
package blob

import (
	"context"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Object is one listing entry.
type Object struct {
	Key  string
	Size int64
}

// Client is the subset of S3/R2 the hub needs.
type Client interface {
	// Put stores an object. size is the exact body length when known
	// (pass -1 to let the SDK chunk-sign).
	Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error
	// Get fetches an object body; caller closes.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// PresignGet returns a time-limited GET URL.
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	// List returns every object under prefix (paginated).
	List(ctx context.Context, prefix string) ([]Object, error)
}

type r2 struct {
	s3      *s3.Client
	presign *s3.PresignClient
	bucket  string
}

// NewR2 builds a Client for Cloudflare R2 (or any S3-compatible endpoint).
// endpoint is e.g. https://<accountid>.r2.cloudflarestorage.com.
func NewR2(endpoint, bucket, accessKey, secretKey string) Client {
	cli := s3.New(s3.Options{
		Region:       "auto", // R2 ignores region but requires one
		BaseEndpoint: aws.String(endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		UsePathStyle: true,
	})
	return &r2{s3: cli, presign: s3.NewPresignClient(cli), bucket: bucket}
}

func (c *r2) Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	in := &s3.PutObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
		Body:   body,
	}
	if size >= 0 {
		in.ContentLength = aws.Int64(size)
	}
	if contentType != "" {
		in.ContentType = aws.String(contentType)
	}
	_, err := c.s3.PutObject(ctx, in)
	if err != nil {
		return fmt.Errorf("r2 put %s: %w", key, err)
	}
	return nil
}

func (c *r2) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := c.s3.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("r2 get %s: %w", key, err)
	}
	return out.Body, nil
}

func (c *r2) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	req, err := c.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	}, func(o *s3.PresignOptions) { o.Expires = ttl })
	if err != nil {
		return "", fmt.Errorf("r2 presign %s: %w", key, err)
	}
	return req.URL, nil
}

func (c *r2) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	var token *string
	for {
		page, err := c.s3.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(c.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("r2 list %s: %w", prefix, err)
		}
		for _, o := range page.Contents {
			out = append(out, Object{Key: aws.ToString(o.Key), Size: aws.ToInt64(o.Size)})
		}
		if page.IsTruncated == nil || !*page.IsTruncated {
			return out, nil
		}
		token = page.NextContinuationToken
	}
}

// Discard is a dev-mode Client: puts are logged and dropped, reads fail.
// It exists so the hub runs without R2 credentials while developing.
type Discard struct{}

func (Discard) Put(_ context.Context, key string, body io.Reader, size int64, _ string) error {
	n, _ := io.Copy(io.Discard, body)
	log.Printf("blob.Discard: put %s (%d bytes, declared %d)", key, n, size)
	return nil
}

func (Discard) Get(_ context.Context, key string) (io.ReadCloser, error) {
	return nil, fmt.Errorf("blob.Discard: no objects (key %s)", key)
}

func (Discard) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "", fmt.Errorf("blob.Discard: no presign (key %s)", key)
}

func (Discard) List(_ context.Context, prefix string) ([]Object, error) {
	log.Printf("blob.Discard: list %s → empty", prefix)
	return nil, nil
}
