//go:build unit

package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

type mediaBridgeStorageTestEncryptor struct{}

func (mediaBridgeStorageTestEncryptor) Encrypt(plaintext string) (string, error) {
	return "sealed:" + base64.RawStdEncoding.EncodeToString([]byte(plaintext)), nil
}

func (mediaBridgeStorageTestEncryptor) Decrypt(ciphertext string) (string, error) {
	encoded, ok := strings.CutPrefix(ciphertext, "sealed:")
	if !ok {
		return "", errors.New("not encrypted")
	}
	decoded, err := base64.RawStdEncoding.DecodeString(encoded)
	return string(decoded), err
}

type flakyMediaBridgeStorageSettingRepo struct {
	SettingRepository
	base    *mediaBridgeSettingRepo
	getErr  error
	getCall int
}

func (r *flakyMediaBridgeStorageSettingRepo) GetValue(ctx context.Context, key string) (string, error) {
	r.getCall++
	if r.getErr != nil {
		return "", r.getErr
	}
	return r.base.GetValue(ctx, key)
}

func (r *flakyMediaBridgeStorageSettingRepo) Set(ctx context.Context, key, value string) error {
	return r.base.Set(ctx, key, value)
}

type mediaBridgeStorageTestStore struct {
	id        string
	payload   []byte
	deleted   bool
	signedURL string
}

func (s *mediaBridgeStorageTestStore) NewObjectKey(relativePrefix, namespace, extension string) (string, error) {
	return strings.Trim(relativePrefix, "/") + namespace + "/probe" + extension, nil
}

func (s *mediaBridgeStorageTestStore) Put(_ context.Context, _ string, _ string, _ int64, body io.Reader) error {
	payload, err := io.ReadAll(body)
	s.payload = payload
	s.deleted = false
	return err
}

func (s *mediaBridgeStorageTestStore) PresignGet(context.Context, string, time.Duration) (string, error) {
	if s.signedURL != "" {
		return s.signedURL, nil
	}
	return "https://media.example.test/probe?signature=test", nil
}

func (s *mediaBridgeStorageTestStore) Head(context.Context, string) (TemporaryMediaObjectMetadata, error) {
	return TemporaryMediaObjectMetadata{SizeBytes: int64(len(s.payload))}, nil
}

func (s *mediaBridgeStorageTestStore) Delete(context.Context, string) error {
	s.deleted = true
	return nil
}

func newMediaBridgeStorageRuntimeFixture(
	t *testing.T,
	fixedKey bool,
) (*MediaBridgeStorageRuntime, *mediaBridgeSettingRepo, *[]MediaBridgeStorageRuntimeConfig, *[]*mediaBridgeStorageTestStore) {
	t.Helper()
	repo := &mediaBridgeSettingRepo{}
	encryptor := mediaBridgeStorageTestEncryptor{}
	backup := NewBackupService(repo, &config.Config{Totp: config.TotpConfig{EncryptionKeyConfigured: fixedKey}}, encryptor, nil, nil)
	built := make([]MediaBridgeStorageRuntimeConfig, 0)
	stores := make([]*mediaBridgeStorageTestStore, 0)
	factory := func(_ context.Context, cfg MediaBridgeStorageRuntimeConfig) (InlineMediaStore, error) {
		built = append(built, cfg)
		store := &mediaBridgeStorageTestStore{id: cfg.Bucket}
		stores = append(stores, store)
		return store, nil
	}
	return NewMediaBridgeStorageRuntime(repo, encryptor, backup, factory), repo, &built, &stores
}

func validMediaBridgeStorageInput() MediaBridgeStorageUpdateInput {
	return MediaBridgeStorageUpdateInput{
		Provider:        "r2",
		Endpoint:        "https://account.r2.cloudflarestorage.com",
		Region:          "auto",
		Bucket:          "media-private",
		ObjectPrefix:    "media-bridge/",
		AccessKeyID:     "access-key",
		SecretAccessKey: "super-secret",
		ForcePathStyle:  true,
	}
}

func TestMediaBridgeStorageRuntimeEncryptsRedactsAndProbes(t *testing.T) {
	runtime, repo, built, stores := newMediaBridgeStorageRuntimeFixture(t, true)
	ctx := context.Background()

	public, err := runtime.Update(ctx, validMediaBridgeStorageInput())
	require.NoError(t, err)
	require.True(t, public.SecretConfigured)
	require.True(t, public.Ready)
	require.Equal(t, "access-key", public.AccessKeyID)

	raw := repo.values[SettingKeyMediaBridgeStorageSettings]
	require.NotContains(t, raw, "super-secret")
	require.Contains(t, raw, "encrypted_secret_access_key")
	encoded, err := json.Marshal(public)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "secret_access_key")

	require.Len(t, *built, 1)
	require.Equal(t, "super-secret", (*built)[0].SecretAccessKey())
	require.Len(t, *stores, 1)
	require.NotEmpty(t, (*stores)[0].payload)
	require.True(t, (*stores)[0].deleted)
}

func TestMediaBridgeStorageRuntimeBlankSecretPreservesAndHotSwitches(t *testing.T) {
	runtime, _, built, _ := newMediaBridgeStorageRuntimeFixture(t, true)
	ctx := context.Background()
	_, err := runtime.Update(ctx, validMediaBridgeStorageInput())
	require.NoError(t, err)
	oldStore, err := runtime.SnapshotStore(ctx)
	require.NoError(t, err)

	updated := validMediaBridgeStorageInput()
	updated.Bucket = "media-private-next"
	updated.SecretAccessKey = ""
	_, err = runtime.Update(ctx, updated)
	require.NoError(t, err)
	newStore, err := runtime.SnapshotStore(ctx)
	require.NoError(t, err)

	require.NotSame(t, oldStore, newStore)
	require.Equal(t, "super-secret", (*built)[1].SecretAccessKey())
	require.Equal(t, "media-private", oldStore.(*mediaBridgeStorageTestStore).id)
	require.Equal(t, "media-private-next", newStore.(*mediaBridgeStorageTestStore).id)
}

func TestMediaBridgeStorageRuntimeRejectsDurableSecretWithoutFixedKey(t *testing.T) {
	runtime, repo, _, _ := newMediaBridgeStorageRuntimeFixture(t, false)

	_, err := runtime.Update(context.Background(), validMediaBridgeStorageInput())

	require.ErrorIs(t, err, ErrSecretEncryptionKeyNotConfigured)
	require.Empty(t, repo.values[SettingKeyMediaBridgeStorageSettings])
}

func TestMediaBridgeStorageRuntimeTestDoesNotPublish(t *testing.T) {
	runtime, repo, _, stores := newMediaBridgeStorageRuntimeFixture(t, true)

	public, err := runtime.Test(context.Background(), validMediaBridgeStorageInput())
	require.NoError(t, err)
	require.True(t, public.Ready)
	require.True(t, public.SecretConfigured)
	require.Empty(t, repo.values[SettingKeyMediaBridgeStorageSettings])
	_, err = runtime.SnapshotStore(context.Background())
	require.ErrorIs(t, err, ErrTemporaryMediaUnavailable)
	require.Len(t, *stores, 1)
}

func TestProbeMediaBridgeInlineStoreRejectsSignedURLOnNonHTTPSPort(t *testing.T) {
	store := &mediaBridgeStorageTestStore{
		signedURL: "https://media.example.test:8443/probe?signature=test",
	}

	err := probeMediaBridgeInlineStore(context.Background(), store)

	require.ErrorContains(t, err, "unsupported authority")
	require.True(t, store.deleted, "failed probes must still delete their temporary object")
}

func TestMediaBridgeStorageRuntimeUsesShortStaleCacheDuringDBFailure(t *testing.T) {
	base := &mediaBridgeSettingRepo{}
	repo := &flakyMediaBridgeStorageSettingRepo{base: base}
	encryptor := mediaBridgeStorageTestEncryptor{}
	backup := NewBackupService(repo, &config.Config{Totp: config.TotpConfig{EncryptionKeyConfigured: true}}, encryptor, nil, nil)
	factory := func(_ context.Context, cfg MediaBridgeStorageRuntimeConfig) (InlineMediaStore, error) {
		return &mediaBridgeStorageTestStore{id: cfg.Bucket}, nil
	}
	runtime := NewMediaBridgeStorageRuntime(repo, encryptor, backup, factory)

	_, err := runtime.Update(context.Background(), validMediaBridgeStorageInput())
	require.NoError(t, err)
	previous := runtime.cache.Load()
	require.NotNil(t, previous)
	expired := *previous
	expired.expiresAt = time.Now().Add(-time.Second).UnixNano()
	runtime.cache.Store(&expired)

	getCallsBeforeFailure := repo.getCall
	repo.getErr = errors.New("database unavailable")
	first, err := runtime.SnapshotStore(context.Background())
	require.NoError(t, err)
	second, err := runtime.SnapshotStore(context.Background())
	require.NoError(t, err)
	require.Same(t, first, second)
	require.Equal(t, getCallsBeforeFailure+1, repo.getCall, "error TTL must prevent a request convoy")
}
