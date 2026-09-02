package repository

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/require"
)

func TestTemporaryMediaR2PresignGetUsesSigV4(t *testing.T) {
	client, err := newS3Client(context.Background(), s3ClientParams{
		Endpoint:        "https://account-id.r2.cloudflarestorage.com",
		Region:          "auto",
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "test-secret-that-must-not-appear",
		ForcePathStyle:  true,
	})
	require.NoError(t, err)
	store := &temporaryMediaR2Store{
		client:  client,
		presign: s3.NewPresignClient(client),
		bucket:  "private-media",
		keyRoot: "media-bridge/",
	}

	signed, err := store.PresignGet(context.Background(), "media-bridge/chat-video/20260902/object.mp4", 15*time.Minute)
	require.NoError(t, err)
	parsed, err := url.Parse(signed)
	require.NoError(t, err)
	require.Equal(t, "AWS4-HMAC-SHA256", parsed.Query().Get("X-Amz-Algorithm"))
	require.Equal(t, "900", parsed.Query().Get("X-Amz-Expires"))
	require.NotEmpty(t, parsed.Query().Get("X-Amz-Signature"))
	require.NotContains(t, signed, "test-secret-that-must-not-appear")
}

func TestTemporaryMediaR2PutStreamsInlineMedia(t *testing.T) {
	var received []byte
	var receivedLength int64
	var receivedCacheControl string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPut, r.Method)
		receivedLength = r.ContentLength
		receivedCacheControl = r.Header.Get("Cache-Control")
		var err error
		received, err = io.ReadAll(r.Body)
		require.NoError(t, err)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := newS3Client(context.Background(), s3ClientParams{
		Endpoint:        server.URL,
		Region:          "auto",
		AccessKeyID:     "test-ak",
		SecretAccessKey: "test-sk",
		ForcePathStyle:  true,
	})
	require.NoError(t, err)
	store := &temporaryMediaR2Store{client: client, bucket: "private-media", keyRoot: "media-bridge/"}
	payload := []byte("streamed-mp4-payload")
	err = store.Put(context.Background(), "media-bridge/chat-video/20260902/object.mp4", "video/mp4", int64(len(payload)), bytes.NewReader(payload))
	require.NoError(t, err)
	require.Equal(t, int64(len(payload)), receivedLength)
	require.Equal(t, payload, received)
	require.Equal(t, "private, no-store, max-age=0", receivedCacheControl)
}

func TestTemporaryMediaR2NewObjectKeyUsesConfiguredStoreRoot(t *testing.T) {
	store := &temporaryMediaR2Store{keyRoot: "media-bridge/"}
	key, err := store.NewObjectKey("", "chat-video", ".mp4")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(key, "media-bridge/chat-video/"))
	require.True(t, strings.HasSuffix(key, ".mp4"))
	require.NoError(t, store.validateKey(key))
}
