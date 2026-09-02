package repository

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// temporaryMediaR2Store is the minimum R2 implementation shared by Media
// Bridge. Legacy direct-upload/presign APIs are deliberately not restored on
// the OVH branch.
type temporaryMediaR2Store struct {
	client  *s3.Client
	presign *s3.PresignClient
	bucket  string
	keyRoot string
}

var _ service.InlineMediaStore = (*temporaryMediaR2Store)(nil)

func (s *temporaryMediaR2Store) NewObjectKey(relativePrefix, namespace, extension string) (string, error) {
	relativePrefix = strings.Trim(strings.TrimSpace(relativePrefix), "/")
	namespace = strings.Trim(strings.TrimSpace(namespace), "/")
	extension = strings.TrimSpace(extension)
	if (relativePrefix != "" && (strings.Contains(relativePrefix, "\\") || path.Clean(relativePrefix) != relativePrefix || strings.Contains(relativePrefix, ".."))) ||
		namespace == "" || strings.Contains(namespace, "\\") || path.Clean(namespace) != namespace ||
		strings.Contains(namespace, "..") || extension == "" || extension[0] != '.' ||
		strings.ContainsAny(extension, "/\\") || len(extension) > 16 {
		return "", service.ErrTemporaryMediaInvalidInput
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate inline media object key: %w", err)
	}
	key := path.Join(strings.TrimSuffix(s.keyRoot, "/"), relativePrefix, namespace, time.Now().UTC().Format("20060102"), hex.EncodeToString(random)+extension)
	if err := s.validateKey(key); err != nil {
		return "", err
	}
	return key, nil
}

func (s *temporaryMediaR2Store) Put(ctx context.Context, key, contentType string, sizeBytes int64, body io.Reader) error {
	if err := s.validateKey(key); err != nil {
		return err
	}
	if strings.TrimSpace(contentType) == "" || sizeBytes <= 0 || body == nil {
		return service.ErrTemporaryMediaInvalidInput
	}
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		ContentType:   aws.String(contentType),
		ContentLength: aws.Int64(sizeBytes),
		CacheControl:  aws.String("private, no-store, max-age=0"),
		Body:          body,
	})
	if err != nil {
		return fmt.Errorf("put inline media object: %w", err)
	}
	return nil
}

func (s *temporaryMediaR2Store) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	if err := s.validateKey(key); err != nil {
		return "", err
	}
	if ttl <= 0 {
		return "", service.ErrTemporaryMediaExpired
	}
	result, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(clampTemporaryMediaPresignTTL(ttl)))
	if err != nil {
		return "", fmt.Errorf("presign temporary media GET: %w", err)
	}
	return result.URL, nil
}

func (s *temporaryMediaR2Store) Head(ctx context.Context, key string) (service.TemporaryMediaObjectMetadata, error) {
	if err := s.validateKey(key); err != nil {
		return service.TemporaryMediaObjectMetadata{}, err
	}
	result, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return service.TemporaryMediaObjectMetadata{}, fmt.Errorf("head temporary media object: %w", err)
	}
	return service.TemporaryMediaObjectMetadata{
		ContentType: aws.ToString(result.ContentType),
		SizeBytes:   aws.ToInt64(result.ContentLength),
	}, nil
}

func (s *temporaryMediaR2Store) Delete(ctx context.Context, key string) error {
	if err := s.validateKey(key); err != nil {
		return err
	}
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return fmt.Errorf("delete temporary media object: %w", err)
	}
	return nil
}

func (s *temporaryMediaR2Store) validateKey(key string) error {
	key = strings.TrimSpace(key)
	if key == "" || len(key) > 512 || strings.HasPrefix(key, "/") || strings.Contains(key, "\\") ||
		path.Clean(key) != key || !strings.HasPrefix(key, s.keyRoot) {
		return errors.Join(service.ErrTemporaryMediaInvalidInput, errors.New("invalid object key"))
	}
	return nil
}

func clampTemporaryMediaPresignTTL(ttl time.Duration) time.Duration {
	const maximum = 7 * 24 * time.Hour
	if ttl > maximum {
		return maximum
	}
	return ttl
}
