package repository

import (
	"context"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// NewMediaBridgeR2InlineStore builds a dedicated runtime store exclusively
// from the encrypted administrator configuration. It deliberately has no
// TEMP_MEDIA_* fallback: an absent or invalid database configuration stays
// fail-closed.
func NewMediaBridgeR2InlineStore(
	ctx context.Context,
	cfg service.MediaBridgeStorageRuntimeConfig,
) (service.InlineMediaStore, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	client, err := newS3Client(ctx, s3ClientParams{
		Endpoint:        cfg.Endpoint,
		Region:          cfg.Region,
		AccessKeyID:     cfg.AccessKeyID(),
		SecretAccessKey: cfg.SecretAccessKey(),
		ForcePathStyle:  cfg.ForcePathStyle,
	})
	if err != nil {
		return nil, fmt.Errorf("create media bridge R2 client: %w", err)
	}
	return &temporaryMediaR2Store{
		client:  client,
		presign: s3.NewPresignClient(client),
		bucket:  cfg.Bucket,
		keyRoot: strings.Trim(cfg.ObjectPrefix, "/") + "/",
	}, nil
}

func ProvideMediaBridgeInlineStoreFactory() service.MediaBridgeInlineStoreFactory {
	return NewMediaBridgeR2InlineStore
}
