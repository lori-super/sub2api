package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const SettingKeyMediaBridgeStorageSettings = "media_bridge_storage_settings"

const (
	mediaBridgeStorageCacheTTL     = 5 * time.Second
	mediaBridgeStorageErrorTTL     = time.Second
	mediaBridgeStorageStaleTTL     = time.Minute
	mediaBridgeStorageLoadTimeout  = 3 * time.Second
	mediaBridgeStorageProbeTTL     = 5 * time.Minute
	mediaBridgeStorageProbeTimeout = 20 * time.Second
)

var (
	ErrMediaBridgeStorageNotConfigured = errors.New("media bridge storage is not configured")
	ErrMediaBridgeStorageProbeFailed   = errors.New("media bridge storage connection test failed")
)

// MediaBridgeStorageUpdateInput is accepted only by the step-up protected
// administrator endpoint. SecretAccessKey is write-only: an empty value keeps
// the previously encrypted secret, and no read API ever returns it.
type MediaBridgeStorageUpdateInput struct {
	Provider        string `json:"provider"`
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region"`
	Bucket          string `json:"bucket"`
	ObjectPrefix    string `json:"object_prefix"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key,omitempty"` //nolint:revive // AWS field name
	ForcePathStyle  bool   `json:"force_path_style"`
}

type persistedMediaBridgeStorageSettings struct {
	Version                  int    `json:"version"`
	Provider                 string `json:"provider"`
	Endpoint                 string `json:"endpoint"`
	Region                   string `json:"region"`
	Bucket                   string `json:"bucket"`
	ObjectPrefix             string `json:"object_prefix"`
	AccessKeyID              string `json:"access_key_id"`
	EncryptedSecretAccessKey string `json:"encrypted_secret_access_key,omitempty"`
	ForcePathStyle           bool   `json:"force_path_style"`
}

// MediaBridgeStorageRuntimeConfig is passed to the repository factory. Secret
// fields stay private and String/GoString always redact them.
type MediaBridgeStorageRuntimeConfig struct {
	Provider       string
	Endpoint       string
	Region         string
	Bucket         string
	ObjectPrefix   string
	ForcePathStyle bool

	accessKeyID     string
	secretAccessKey string
}

func (c MediaBridgeStorageRuntimeConfig) AccessKeyID() string     { return c.accessKeyID }
func (c MediaBridgeStorageRuntimeConfig) SecretAccessKey() string { return c.secretAccessKey }

func (c MediaBridgeStorageRuntimeConfig) String() string {
	return fmt.Sprintf(
		"MediaBridgeStorageRuntimeConfig{Provider:%q Endpoint:%q Region:%q Bucket:%q ObjectPrefix:%q ForcePathStyle:%t Credentials:<redacted>}",
		c.Provider,
		c.Endpoint,
		c.Region,
		c.Bucket,
		c.ObjectPrefix,
		c.ForcePathStyle,
	)
}

func (c MediaBridgeStorageRuntimeConfig) GoString() string { return c.String() }

func (c MediaBridgeStorageRuntimeConfig) Validate() error {
	if c.Provider != "r2" {
		return errors.New("media bridge storage provider must be r2")
	}
	if err := validateTemporaryMediaEndpoint(c.Endpoint); err != nil {
		return errors.New("media bridge storage endpoint must be an HTTPS origin")
	}
	if !validTemporaryMediaBucket(c.Bucket) {
		return errors.New("media bridge storage bucket is invalid")
	}
	if !validTemporaryMediaPrefix(c.ObjectPrefix) {
		return errors.New("media bridge storage object prefix is invalid")
	}
	if strings.TrimSpace(c.accessKeyID) == "" || strings.TrimSpace(c.secretAccessKey) == "" {
		return ErrMediaBridgeStorageNotConfigured
	}
	return nil
}

type MediaBridgeInlineStoreFactory func(context.Context, MediaBridgeStorageRuntimeConfig) (InlineMediaStore, error)

type cachedMediaBridgeInlineStore struct {
	rawHash    [sha256.Size]byte
	settings   MediaBridgeStorageSettings
	store      InlineMediaStore
	expiresAt  int64
	staleUntil int64
	refreshErr error
}

// MediaBridgeStorageRuntime owns the encrypted administrator configuration and
// atomically publishes immutable stores. A request takes one Store snapshot;
// later administrator updates affect only subsequent requests.
type MediaBridgeStorageRuntime struct {
	settingRepo SettingRepository
	encryptor   SecretEncryptor
	backup      *BackupService
	factory     MediaBridgeInlineStoreFactory
	probeClient *http.Client

	updateMu sync.Mutex
	mu       sync.Mutex
	cache    atomic.Pointer[cachedMediaBridgeInlineStore]
}

func NewMediaBridgeStorageRuntime(
	settingRepo SettingRepository,
	encryptor SecretEncryptor,
	backup *BackupService,
	factory MediaBridgeInlineStoreFactory,
) *MediaBridgeStorageRuntime {
	return &MediaBridgeStorageRuntime{
		settingRepo: settingRepo,
		encryptor:   encryptor,
		backup:      backup,
		factory:     factory,
		probeClient: &http.Client{
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Get returns only non-secret administrator fields and a live readiness bit.
func (s *MediaBridgeStorageRuntime) Get(ctx context.Context) (MediaBridgeStorageSettings, error) {
	cached, err := s.refresh(ctx, true)
	if err != nil {
		if errors.Is(err, ErrMediaBridgeStorageNotConfigured) {
			return defaultMediaBridgeStorageSettings(), nil
		}
		return MediaBridgeStorageSettings{}, err
	}
	return cloneMediaBridgeStorageSettings(cached.settings), nil
}

// SnapshotStore returns the immutable store for one new bridge request. It
// never falls back to TEMP_MEDIA_* or to a previously failed configuration.
func (s *MediaBridgeStorageRuntime) SnapshotStore(ctx context.Context) (InlineMediaStore, error) {
	cached, err := s.refresh(ctx, false)
	if err != nil || cached == nil || cached.store == nil || !cached.settings.Ready {
		return nil, ErrTemporaryMediaUnavailable
	}
	return cached.store, nil
}

// Update validates, builds, probes and persists a candidate before publishing
// it. SecretAccessKey="" retains the encrypted value already in the database.
func (s *MediaBridgeStorageRuntime) Update(
	ctx context.Context,
	input MediaBridgeStorageUpdateInput,
) (MediaBridgeStorageSettings, error) {
	if s == nil || s.settingRepo == nil || s.encryptor == nil || s.factory == nil {
		return MediaBridgeStorageSettings{}, ErrTemporaryMediaUnavailable
	}
	input, err := normalizeMediaBridgeStorageUpdate(input)
	if err != nil {
		return MediaBridgeStorageSettings{}, err
	}

	// Serialize administrator writes, but do not hold the cache-refresh mutex
	// across the real network probe. Existing/new bridge requests can keep
	// resolving the last published store while a candidate is tested.
	s.updateMu.Lock()
	defer s.updateMu.Unlock()

	prior, _, err := s.loadPersisted(ctx)
	if err != nil && !errors.Is(err, ErrMediaBridgeStorageNotConfigured) {
		return MediaBridgeStorageSettings{}, err
	}
	if prior == nil {
		prior = &persistedMediaBridgeStorageSettings{Version: 1}
	}

	secretCiphertext := prior.EncryptedSecretAccessKey
	secretPlaintext := strings.TrimSpace(input.SecretAccessKey)
	if secretPlaintext == "" {
		if secretCiphertext == "" {
			return MediaBridgeStorageSettings{}, errors.New("storage.secret_access_key is required")
		}
		secretPlaintext, err = s.encryptor.Decrypt(secretCiphertext)
		if err != nil {
			return MediaBridgeStorageSettings{}, errors.New("decrypt media bridge storage secret")
		}
	} else {
		if s.backup == nil || !s.backup.EncryptionKeyConfigured() {
			return MediaBridgeStorageSettings{}, ErrSecretEncryptionKeyNotConfigured
		}
		secretCiphertext, err = s.encryptor.Encrypt(secretPlaintext)
		if err != nil {
			return MediaBridgeStorageSettings{}, fmt.Errorf("encrypt media bridge storage secret: %w", err)
		}
	}

	persisted := persistedMediaBridgeStorageSettings{
		Version:                  1,
		Provider:                 input.Provider,
		Endpoint:                 input.Endpoint,
		Region:                   input.Region,
		Bucket:                   input.Bucket,
		ObjectPrefix:             input.ObjectPrefix,
		AccessKeyID:              input.AccessKeyID,
		EncryptedSecretAccessKey: secretCiphertext,
		ForcePathStyle:           input.ForcePathStyle,
	}
	config, err := mediaBridgeStorageRuntimeConfig(persisted, secretPlaintext)
	if err != nil {
		return MediaBridgeStorageSettings{}, err
	}
	store, err := s.factory(ctx, config)
	if err != nil || store == nil {
		return MediaBridgeStorageSettings{}, errors.Join(ErrMediaBridgeStorageProbeFailed, err)
	}
	if err := probeMediaBridgeInlineStore(ctx, store, s.probeClient); err != nil {
		return MediaBridgeStorageSettings{}, errors.Join(ErrMediaBridgeStorageProbeFailed, err)
	}

	raw, err := json.Marshal(persisted)
	if err != nil {
		return MediaBridgeStorageSettings{}, fmt.Errorf("marshal media bridge storage settings: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.settingRepo.Set(ctx, SettingKeyMediaBridgeStorageSettings, string(raw)); err != nil {
		return MediaBridgeStorageSettings{}, fmt.Errorf("save media bridge storage settings: %w", err)
	}
	public := publicMediaBridgeStorageSettings(persisted, true)
	s.cache.Store(&cachedMediaBridgeInlineStore{
		rawHash:    sha256.Sum256(raw),
		settings:   public,
		store:      store,
		expiresAt:  time.Now().Add(mediaBridgeStorageCacheTTL).UnixNano(),
		staleUntil: time.Now().Add(mediaBridgeStorageStaleTTL).UnixNano(),
	})
	return public, nil
}

// Test builds and probes a candidate without persisting or publishing it.
func (s *MediaBridgeStorageRuntime) Test(
	ctx context.Context,
	input MediaBridgeStorageUpdateInput,
) (MediaBridgeStorageSettings, error) {
	if s == nil || s.settingRepo == nil || s.encryptor == nil || s.factory == nil {
		return MediaBridgeStorageSettings{}, ErrTemporaryMediaUnavailable
	}
	input, err := normalizeMediaBridgeStorageUpdate(input)
	if err != nil {
		return MediaBridgeStorageSettings{}, err
	}
	secret := strings.TrimSpace(input.SecretAccessKey)
	if secret == "" {
		prior, _, loadErr := s.loadPersisted(ctx)
		if loadErr != nil || prior == nil || prior.EncryptedSecretAccessKey == "" {
			return MediaBridgeStorageSettings{}, errors.New("storage.secret_access_key is required")
		}
		secret, err = s.encryptor.Decrypt(prior.EncryptedSecretAccessKey)
		if err != nil {
			return MediaBridgeStorageSettings{}, errors.New("decrypt media bridge storage secret")
		}
	}
	persisted := persistedMediaBridgeStorageSettings{
		Version:        1,
		Provider:       input.Provider,
		Endpoint:       input.Endpoint,
		Region:         input.Region,
		Bucket:         input.Bucket,
		ObjectPrefix:   input.ObjectPrefix,
		AccessKeyID:    input.AccessKeyID,
		ForcePathStyle: input.ForcePathStyle,
	}
	config, err := mediaBridgeStorageRuntimeConfig(persisted, secret)
	if err != nil {
		return MediaBridgeStorageSettings{}, err
	}
	store, err := s.factory(ctx, config)
	if err != nil || store == nil {
		return MediaBridgeStorageSettings{}, errors.Join(ErrMediaBridgeStorageProbeFailed, err)
	}
	if err := probeMediaBridgeInlineStore(ctx, store, s.probeClient); err != nil {
		return MediaBridgeStorageSettings{}, errors.Join(ErrMediaBridgeStorageProbeFailed, err)
	}
	public := publicMediaBridgeStorageSettings(persisted, true)
	public.SecretConfigured = true
	return public, nil
}

func (s *MediaBridgeStorageRuntime) refresh(
	ctx context.Context,
	force bool,
) (*cachedMediaBridgeInlineStore, error) {
	if s == nil || s.settingRepo == nil || s.encryptor == nil || s.factory == nil {
		return nil, ErrMediaBridgeStorageNotConfigured
	}
	if !force {
		if cached := s.cache.Load(); cached != nil && time.Now().UnixNano() < cached.expiresAt {
			return mediaBridgeStorageCachedResult(cached)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !force {
		if cached := s.cache.Load(); cached != nil && time.Now().UnixNano() < cached.expiresAt {
			return mediaBridgeStorageCachedResult(cached)
		}
	}

	if ctx == nil {
		ctx = context.Background()
	}
	loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mediaBridgeStorageLoadTimeout)
	defer cancel()
	persisted, raw, err := s.loadPersisted(loadCtx)
	if err != nil {
		if force {
			return nil, err
		}
		now := time.Now()
		if !errors.Is(err, ErrMediaBridgeStorageNotConfigured) {
			if stale := s.cache.Load(); stale != nil && stale.store != nil && stale.settings.Ready && now.UnixNano() < stale.staleUntil {
				fallback := &cachedMediaBridgeInlineStore{
					rawHash:    stale.rawHash,
					settings:   cloneMediaBridgeStorageSettings(stale.settings),
					store:      stale.store,
					expiresAt:  now.Add(mediaBridgeStorageErrorTTL).UnixNano(),
					staleUntil: stale.staleUntil,
				}
				s.cache.Store(fallback)
				return fallback, nil
			}
		}
		s.cache.Store(&cachedMediaBridgeInlineStore{
			expiresAt:  now.Add(mediaBridgeStorageErrorTTL).UnixNano(),
			refreshErr: err,
		})
		return nil, err
	}
	hash := sha256.Sum256(raw)
	if cached := s.cache.Load(); cached != nil && cached.rawHash == hash && cached.store != nil {
		refreshed := &cachedMediaBridgeInlineStore{
			rawHash:    cached.rawHash,
			settings:   cloneMediaBridgeStorageSettings(cached.settings),
			store:      cached.store,
			expiresAt:  time.Now().Add(mediaBridgeStorageCacheTTL).UnixNano(),
			staleUntil: time.Now().Add(mediaBridgeStorageStaleTTL).UnixNano(),
		}
		s.cache.Store(refreshed)
		return refreshed, nil
	}
	secret, err := s.encryptor.Decrypt(persisted.EncryptedSecretAccessKey)
	if err != nil {
		s.cache.Store(nil)
		return nil, errors.New("decrypt media bridge storage secret")
	}
	config, err := mediaBridgeStorageRuntimeConfig(*persisted, secret)
	if err != nil {
		s.cache.Store(nil)
		return nil, err
	}
	store, err := s.factory(loadCtx, config)
	if err != nil || store == nil {
		s.cache.Store(nil)
		return nil, errors.Join(ErrTemporaryMediaUnavailable, err)
	}
	resolved := &cachedMediaBridgeInlineStore{
		rawHash:    hash,
		settings:   publicMediaBridgeStorageSettings(*persisted, true),
		store:      store,
		expiresAt:  time.Now().Add(mediaBridgeStorageCacheTTL).UnixNano(),
		staleUntil: time.Now().Add(mediaBridgeStorageStaleTTL).UnixNano(),
	}
	s.cache.Store(resolved)
	return resolved, nil
}

func mediaBridgeStorageCachedResult(cached *cachedMediaBridgeInlineStore) (*cachedMediaBridgeInlineStore, error) {
	if cached == nil {
		return nil, ErrMediaBridgeStorageNotConfigured
	}
	if cached.refreshErr != nil {
		return nil, cached.refreshErr
	}
	return cached, nil
}

func (s *MediaBridgeStorageRuntime) loadPersisted(
	ctx context.Context,
) (*persistedMediaBridgeStorageSettings, []byte, error) {
	raw, err := s.settingRepo.GetValue(ctx, SettingKeyMediaBridgeStorageSettings)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return nil, nil, ErrMediaBridgeStorageNotConfigured
		}
		return nil, nil, fmt.Errorf("load media bridge storage settings: %w", err)
	}
	if strings.TrimSpace(raw) == "" {
		return nil, nil, ErrMediaBridgeStorageNotConfigured
	}
	var persisted persistedMediaBridgeStorageSettings
	if err := json.Unmarshal([]byte(raw), &persisted); err != nil {
		return nil, nil, errors.New("media bridge storage settings are invalid")
	}
	if persisted.Version != 1 || persisted.EncryptedSecretAccessKey == "" {
		return nil, nil, ErrMediaBridgeStorageNotConfigured
	}
	input := MediaBridgeStorageUpdateInput{
		Provider:       persisted.Provider,
		Endpoint:       persisted.Endpoint,
		Region:         persisted.Region,
		Bucket:         persisted.Bucket,
		ObjectPrefix:   persisted.ObjectPrefix,
		AccessKeyID:    persisted.AccessKeyID,
		ForcePathStyle: persisted.ForcePathStyle,
	}
	normalized, err := normalizeMediaBridgeStorageUpdate(input)
	if err != nil {
		return nil, nil, errors.New("media bridge storage settings are unsafe")
	}
	persisted.Provider = normalized.Provider
	persisted.Endpoint = normalized.Endpoint
	persisted.Region = normalized.Region
	persisted.Bucket = normalized.Bucket
	persisted.ObjectPrefix = normalized.ObjectPrefix
	persisted.AccessKeyID = normalized.AccessKeyID
	return &persisted, []byte(raw), nil
}

func normalizeMediaBridgeStorageUpdate(input MediaBridgeStorageUpdateInput) (MediaBridgeStorageUpdateInput, error) {
	input.Provider = strings.ToLower(strings.TrimSpace(input.Provider))
	if input.Provider == "" {
		input.Provider = "r2"
	}
	if input.Provider != "r2" {
		return input, errors.New("storage.provider must be r2")
	}
	input.Endpoint = strings.TrimRight(strings.TrimSpace(input.Endpoint), "/")
	if err := validateTemporaryMediaEndpoint(input.Endpoint); err != nil {
		return input, errors.New("storage.endpoint must be an HTTPS origin")
	}
	parsed, _ := url.Parse(input.Endpoint)
	if parsed == nil || parsed.Path != "" {
		return input, errors.New("storage.endpoint must not contain a path")
	}
	input.Region = strings.TrimSpace(input.Region)
	if input.Region == "" {
		input.Region = "auto"
	}
	if len(input.Region) > 128 || strings.ContainsAny(input.Region, "\r\n\x00") {
		return input, errors.New("storage.region is invalid")
	}
	input.Bucket = strings.TrimSpace(input.Bucket)
	if !validTemporaryMediaBucket(input.Bucket) {
		return input, errors.New("storage.bucket is invalid")
	}
	input.ObjectPrefix = strings.Trim(strings.TrimSpace(input.ObjectPrefix), "/")
	if input.ObjectPrefix == "" {
		input.ObjectPrefix = "media-bridge"
	}
	if !validTemporaryMediaPrefix(input.ObjectPrefix) {
		return input, errors.New("storage.object_prefix is invalid")
	}
	input.AccessKeyID = strings.TrimSpace(input.AccessKeyID)
	if input.AccessKeyID == "" || len(input.AccessKeyID) > 1024 || strings.ContainsAny(input.AccessKeyID, "\r\n\x00") {
		return input, errors.New("storage.access_key_id is invalid")
	}
	input.SecretAccessKey = strings.TrimSpace(input.SecretAccessKey)
	if len(input.SecretAccessKey) > 4096 || strings.ContainsAny(input.SecretAccessKey, "\r\n\x00") {
		return input, errors.New("storage.secret_access_key is invalid")
	}
	return input, nil
}

func mediaBridgeStorageRuntimeConfig(
	persisted persistedMediaBridgeStorageSettings,
	secret string,
) (MediaBridgeStorageRuntimeConfig, error) {
	config := MediaBridgeStorageRuntimeConfig{
		Provider:        persisted.Provider,
		Endpoint:        persisted.Endpoint,
		Region:          persisted.Region,
		Bucket:          persisted.Bucket,
		ObjectPrefix:    strings.Trim(persisted.ObjectPrefix, "/"),
		ForcePathStyle:  persisted.ForcePathStyle,
		accessKeyID:     persisted.AccessKeyID,
		secretAccessKey: secret,
	}
	if err := config.Validate(); err != nil {
		return MediaBridgeStorageRuntimeConfig{}, err
	}
	return config, nil
}

func publicMediaBridgeStorageSettings(
	persisted persistedMediaBridgeStorageSettings,
	ready bool,
) MediaBridgeStorageSettings {
	prefix := strings.Trim(persisted.ObjectPrefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	return MediaBridgeStorageSettings{
		Provider:         persisted.Provider,
		Endpoint:         persisted.Endpoint,
		Region:           persisted.Region,
		Bucket:           persisted.Bucket,
		ObjectPrefix:     prefix,
		AccessKeyID:      persisted.AccessKeyID,
		ForcePathStyle:   persisted.ForcePathStyle,
		SecretConfigured: persisted.EncryptedSecretAccessKey != "",
		Ready:            ready,
	}
}

func defaultMediaBridgeStorageSettings() MediaBridgeStorageSettings {
	return MediaBridgeStorageSettings{
		Provider:       "r2",
		Region:         "auto",
		ObjectPrefix:   "media-bridge/",
		ForcePathStyle: true,
	}
}

func cloneMediaBridgeStorageSettings(settings MediaBridgeStorageSettings) MediaBridgeStorageSettings {
	return settings
}

type mediaBridgeInlineStoreHead interface {
	Head(context.Context, string) (TemporaryMediaObjectMetadata, error)
}

func probeMediaBridgeInlineStore(ctx context.Context, store InlineMediaStore, client *http.Client) error {
	if store == nil {
		return ErrTemporaryMediaUnavailable
	}
	if client == nil {
		client = http.DefaultClient
	}
	if ctx == nil {
		ctx = context.Background()
	}
	probeCtx, cancel := context.WithTimeout(ctx, mediaBridgeStorageProbeTimeout)
	defer cancel()

	key, err := store.NewObjectKey("", "media-bridge-probe", ".bin")
	if err != nil {
		return fmt.Errorf("create probe object key: %w", err)
	}
	payload := []byte("worldcodes-media-bridge-r2-probe")
	putAttempted := false
	deleted := false
	defer func() {
		if !putAttempted || deleted {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_ = store.Delete(cleanupCtx, key)
	}()

	putAttempted = true
	if err := store.Put(probeCtx, key, "application/octet-stream", int64(len(payload)), bytes.NewReader(payload)); err != nil {
		return fmt.Errorf("put probe object: %w", err)
	}
	signedURL, err := store.PresignGet(probeCtx, key, mediaBridgeStorageProbeTTL)
	if err != nil {
		return fmt.Errorf("presign probe object: %w", err)
	}
	if err := validateOpenAIChatVideoAssetURL(signedURL); err != nil {
		return fmt.Errorf("probe returned an unsafe signed URL: %w", err)
	}
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, signedURL, nil)
	if err != nil {
		return fmt.Errorf("create signed URL probe request: %w", err)
	}
	request.Header.Set("Range", "bytes=0-0")
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("download signed URL probe object: %s", sanitizeUpstreamErrorMessage(err.Error()))
	}
	downloading, readErr := io.ReadAll(io.LimitReader(response.Body, int64(len(payload)+1)))
	closeErr := response.Body.Close()
	if readErr != nil {
		return fmt.Errorf("read signed URL probe object: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close signed URL probe object: %w", closeErr)
	}
	if response.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("signed URL range probe returned status %d", response.StatusCode)
	}
	if response.Header.Get("Content-Range") != fmt.Sprintf("bytes 0-0/%d", len(payload)) {
		return errors.New("signed URL range probe returned an invalid Content-Range")
	}
	if !bytes.Equal(downloading, payload[:1]) {
		return errors.New("signed URL range probe content mismatch")
	}
	if headStore, ok := store.(mediaBridgeInlineStoreHead); ok {
		metadata, err := headStore.Head(probeCtx, key)
		if err != nil {
			return fmt.Errorf("head probe object: %w", err)
		}
		if metadata.SizeBytes != int64(len(payload)) {
			return errors.New("probe object size mismatch")
		}
	}
	if err := store.Delete(probeCtx, key); err != nil {
		return fmt.Errorf("delete probe object: %w", err)
	}
	deleted = true
	return nil
}
