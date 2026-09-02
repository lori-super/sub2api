package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

const (
	openAIChatVideoBridgeContentType    = "video/mp4"
	openAIChatVideoBridgeCleanupTimeout = 30 * time.Second
	openAIChatVideoBridgeBodyChunk      = int64(8 << 20)
	openAIChatVideoBridgeJSONBodyFactor = int64(2)
	openAIChatVideoMP4DataURLPrefix     = "data:video/mp4;base64,"
)

var errOpenAIChatVideoUnsupportedMediaType = errors.New("unsupported inline video media type")

// InlineMediaStore is deliberately independent from TemporaryMediaObjectStore.
// Put must consume Body before returning success. PresignGet returns a short-
// lived HTTPS URL for one private object; the bucket itself need not be public.
type InlineMediaStore interface {
	NewObjectKey(relativePrefix, namespace, extension string) (string, error)
	Put(ctx context.Context, key, contentType string, sizeBytes int64, body io.Reader) error
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	Delete(ctx context.Context, key string) error
}

type InlineMediaStoreResolver interface {
	SnapshotStore(context.Context) (InlineMediaStore, error)
}

type unavailableInlineMediaStore struct{}

func NewUnavailableInlineMediaStore() InlineMediaStore { return unavailableInlineMediaStore{} }

func (unavailableInlineMediaStore) NewObjectKey(string, string, string) (string, error) {
	return "", ErrTemporaryMediaUnavailable
}

func (unavailableInlineMediaStore) Put(context.Context, string, string, int64, io.Reader) error {
	return ErrTemporaryMediaUnavailable
}

func (unavailableInlineMediaStore) PresignGet(context.Context, string, time.Duration) (string, error) {
	return "", ErrTemporaryMediaUnavailable
}

func (unavailableInlineMediaStore) Delete(context.Context, string) error {
	return ErrTemporaryMediaUnavailable
}

// OpenAIChatVideoBridgePolicy is an immutable request snapshot. A zero file
// limit means unlimited at this layer; the inbound HTTP body limit still
// applies. Scope lists are exact, case-insensitive matches and empty means no
// additional restriction.
type OpenAIChatVideoBridgePolicy struct {
	Mode                  string
	CanaryPercent         int
	IngressProtocols      []string
	UpstreamProtocols     []string
	Models                []string
	AccountIDs            []int64
	AllowedMIMETypes      []string
	MaxVideoBytes         int64
	MaxRequestBytes       int64
	MaxVideos             int64
	SignedURLTTL          time.Duration
	RequestEndDeleteDelay time.Duration
	Deduplicate           bool
	ObjectPrefix          string
	Capacity              MediaBridgeCapacitySettings
	UploadTimeout         time.Duration
}

type OpenAIChatVideoBridgeRequest struct {
	TenantID         int64
	StableKey        string
	IngressProtocol  string
	UpstreamProtocol string
}

type OpenAIChatVideoBridgePolicyProvider interface {
	SnapshotOpenAIChatVideoBridgePolicy(context.Context, OpenAIChatVideoBridgeRequest) (OpenAIChatVideoBridgePolicy, error)
}

type OpenAIChatVideoBridgePolicyProviderFunc func(context.Context, OpenAIChatVideoBridgeRequest) (OpenAIChatVideoBridgePolicy, error)

func (f OpenAIChatVideoBridgePolicyProviderFunc) SnapshotOpenAIChatVideoBridgePolicy(ctx context.Context, request OpenAIChatVideoBridgeRequest) (OpenAIChatVideoBridgePolicy, error) {
	return f(ctx, request)
}

// NewSettingOpenAIChatVideoBridgePolicyProvider adapts the administrator hot
// cache without making the bridge own or freeze runtime settings.
func NewSettingOpenAIChatVideoBridgePolicyProvider(settings *SettingService) OpenAIChatVideoBridgePolicyProvider {
	return OpenAIChatVideoBridgePolicyProviderFunc(func(ctx context.Context, _ OpenAIChatVideoBridgeRequest) (OpenAIChatVideoBridgePolicy, error) {
		if settings == nil {
			return OpenAIChatVideoBridgePolicy{Mode: MediaBridgeModeOff}, nil
		}
		snapshot := settings.GetMediaBridgeSettingsCached(ctx)
		return OpenAIChatVideoBridgePolicy{
			Mode:                  snapshot.Mode,
			CanaryPercent:         snapshot.CanaryPercent,
			IngressProtocols:      append([]string(nil), snapshot.Scope.IngressProtocols...),
			UpstreamProtocols:     append([]string(nil), snapshot.Scope.UpstreamProtocols...),
			Models:                append([]string(nil), snapshot.Scope.Models...),
			AccountIDs:            append([]int64(nil), snapshot.Scope.AccountIDs...),
			AllowedMIMETypes:      append([]string(nil), snapshot.FilePolicy.AllowedMIMETypes...),
			MaxVideoBytes:         snapshot.FilePolicy.MaxSingleDecodedBytes,
			MaxRequestBytes:       snapshot.FilePolicy.MaxRequestDecodedBytes,
			MaxVideos:             snapshot.FilePolicy.MaxFilesPerRequest,
			SignedURLTTL:          time.Duration(snapshot.Retention.SignedURLTTLSeconds) * time.Second,
			RequestEndDeleteDelay: time.Duration(snapshot.Retention.RequestEndDeleteDelaySeconds) * time.Second,
			Deduplicate:           snapshot.FilePolicy.DeduplicateWithinRequest,
			ObjectPrefix:          snapshot.Storage.ObjectPrefix,
			Capacity:              snapshot.Capacity,
			UploadTimeout:         time.Duration(snapshot.Protection.R2UploadTimeoutSeconds) * time.Second,
		}, nil
	})
}

type OpenAIChatVideoBridge struct {
	store         InlineMediaStore
	storeResolver InlineMediaStoreResolver
	policies      OpenAIChatVideoBridgePolicyProvider
	capacity      *MediaBridgeCapacity
	bodyCapacity  *MediaBridgeCapacity
	r2Circuit     *MediaBridgeR2Circuit
	schedule      func(time.Duration, func())
}

// SetStoreResolver switches new request sessions to administrator-managed
// storage. A session resolves the store only when it finds its first eligible
// inline video, then keeps that exact snapshot for retries and cleanup.
func (b *OpenAIChatVideoBridge) SetStoreResolver(resolver InlineMediaStoreResolver) {
	if b != nil {
		b.storeResolver = resolver
	}
}

func NewOpenAIChatVideoBridge(store InlineMediaStore, policies OpenAIChatVideoBridgePolicyProvider) (*OpenAIChatVideoBridge, error) {
	if store == nil {
		return nil, errors.New("inline media store is required")
	}
	if policies == nil {
		return nil, errors.New("chat video bridge policy provider is required")
	}
	return &OpenAIChatVideoBridge{
		store:        store,
		policies:     policies,
		capacity:     NewMediaBridgeCapacity(),
		bodyCapacity: NewMediaBridgeCapacity(),
		schedule:     func(delay time.Duration, fn func()) { time.AfterFunc(delay, fn) },
	}, nil
}

// SetCapacity installs the process-local admission and upload-rate controller.
// Existing request sessions keep their immutable policy snapshot, while new
// sessions immediately use the controller's current live counters.
func (b *OpenAIChatVideoBridge) SetCapacity(capacity *MediaBridgeCapacity) {
	if b != nil && capacity != nil {
		b.capacity = capacity
	}
}

// SetBodyCapacity installs a separate memory-admission controller for inbound
// request bodies. Business concurrency and upload bandwidth counters remain
// isolated from this heap-safety reservation.
func (b *OpenAIChatVideoBridge) SetBodyCapacity(capacity *MediaBridgeCapacity) {
	if b != nil && capacity != nil {
		b.bodyCapacity = capacity
	}
}

// SetR2Circuit installs the hot-configured R2 health breaker. Admission is
// checked immediately before upload, then every admitted upload reports its
// latency and result so degraded storage is shed before requests pile up.
func (b *OpenAIChatVideoBridge) SetR2Circuit(circuit *MediaBridgeR2Circuit) {
	if b != nil {
		b.r2Circuit = circuit
	}
}

func (s *OpenAIGatewayService) SetOpenAIChatVideoBridge(bridge *OpenAIChatVideoBridge) {
	if s != nil {
		s.chatVideoBridge = bridge
	}
}

const openAIChatVideoBridgeGinSessionKey = "openai_chat_video_bridge_session"

type openAIChatVideoBridgeGinState struct {
	session *OpenAIChatVideoBridgeSession
}

// materializeFinalChatVideoDataURLs is called only by the shared final CC
// sender. The mutable Gin context is request-scoped and shared by Chat,
// Responses→Chat, same-account retries, and account failover.
func (s *OpenAIGatewayService) materializeFinalChatVideoDataURLs(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
) ([]byte, bool, error) {
	upstreamModel := gjson.GetBytes(body, "model").String()
	return s.materializeChatVideoDataURLsForModel(ctx, c, account, upstreamModel, body)
}

// materializeChatVideoDataURLsForModel lets the Chat entry path remove a large
// Data URI before model/policy rewrites allocate another full request-sized
// buffer. The final sender still calls materializeFinalChatVideoDataURLs as a
// safety net for every CC egress.
func (s *OpenAIGatewayService) materializeChatVideoDataURLsForModel(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	upstreamModel string,
	body []byte,
) ([]byte, bool, error) {
	if s == nil || s.chatVideoBridge == nil || c == nil {
		return body, false, nil
	}
	session, err := s.openAIChatVideoBridgeSession(ctx, c)
	if err != nil {
		return nil, false, err
	}
	if session == nil {
		return body, false, nil
	}
	if !session.policy.eligible(account, upstreamModel) {
		return body, false, nil
	}
	return session.materialize(ctx, body)
}

// materializeResponsesVideoDataURLs shrinks inline Responses video payloads
// before the Responses -> Chat adapter unmarshals the full request. This keeps
// large base64 strings out of the adapter's map/struct conversion pipeline;
// the final Chat sender remains the safety net for native Chat requests.
func (s *OpenAIGatewayService) materializeResponsesVideoDataURLs(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	upstreamModel string,
	body []byte,
) ([]byte, bool, error) {
	if s == nil || s.chatVideoBridge == nil || c == nil ||
		openAIChatVideoBridgeIngressProtocol(c) != MediaBridgeIngressOpenAIResponses ||
		!openAIResponsesBodyHasVideo(body) {
		return body, false, nil
	}
	session, err := s.openAIChatVideoBridgeSession(ctx, c)
	if err != nil {
		return nil, false, err
	}
	if session == nil || !session.policy.eligible(account, upstreamModel) {
		return body, false, nil
	}
	return session.materializeResponses(ctx, body)
}

// shouldForceChatVideoEgress keeps the client-facing protocol independent from
// K3's verified video transport. It only selects Chat egress when the request
// contains an explicit video block and the administrator policy applies.
func (s *OpenAIGatewayService) shouldForceChatVideoEgress(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	upstreamModel string,
	body []byte,
) (bool, error) {
	ingressProtocol := openAIChatVideoBridgeIngressProtocol(c)
	if s == nil || s.chatVideoBridge == nil || c == nil ||
		!openAIChatVideoBridgeBodyHasVideo(body, ingressProtocol) {
		return false, nil
	}
	session, err := s.openAIChatVideoBridgeSession(ctx, c)
	if err != nil {
		return false, err
	}
	if session == nil || !session.policy.eligible(account, upstreamModel) {
		return false, nil
	}
	if err := validateOpenAIChatVideoForcedEgress(c, body, ingressProtocol); err != nil {
		return false, newOpenAIChatVideoBridgeError(
			http.StatusBadRequest,
			"invalid_request_error",
			err.Error(),
			err,
		)
	}
	return true, nil
}

func validateOpenAIChatVideoForcedEgress(c *gin.Context, body []byte, ingressProtocol string) error {
	switch strings.ToLower(strings.TrimSpace(ingressProtocol)) {
	case MediaBridgeIngressOpenAIChatCompletions:
		messages := gjson.GetBytes(body, "messages")
		for _, message := range messages.Array() {
			for _, part := range message.Get("content").Array() {
				rawType := strings.TrimSpace(part.Get("type").String())
				if strings.EqualFold(rawType, "video_url") && rawType != "video_url" {
					return errors.New("video content type must be exactly video_url")
				}
			}
		}
		return nil
	case MediaBridgeIngressOpenAIResponses:
		if isOpenAINativeCompactionV2(c) || isOpenAIResponsesCompactPath(c) {
			return errors.New("video input cannot be combined with a Responses compaction request")
		}
		if strings.TrimSpace(gjson.GetBytes(body, "previous_response_id").String()) != "" {
			return errors.New("video input with previous_response_id is not supported by the Chat compatibility route")
		}
		if err := validateOpenAIResponsesVideoInputCompatibility(gjson.GetBytes(body, "input")); err != nil {
			return err
		}
		if err := validateOpenAIResponsesVideoChatTools(gjson.GetBytes(body, "tools")); err != nil {
			return err
		}
		for _, item := range gjson.GetBytes(body, "input").Array() {
			if strings.TrimSpace(item.Get("type").String()) == "additional_tools" {
				if err := validateOpenAIResponsesVideoChatTools(item.Get("tools")); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateOpenAIResponsesVideoInputCompatibility(input gjson.Result) error {
	if !input.IsArray() {
		return nil
	}
	for _, item := range input.Array() {
		if err := validateOpenAIResponsesVideoPartCompatibility(item); err != nil {
			return err
		}
		for _, part := range item.Get("content").Array() {
			if err := validateOpenAIResponsesVideoPartCompatibility(part); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateOpenAIResponsesVideoPartCompatibility(part gjson.Result) error {
	rawType := strings.TrimSpace(part.Get("type").String())
	normalizedType := strings.ToLower(rawType)
	switch normalizedType {
	case "input_video", "video_url", "input_file":
		if rawType != normalizedType {
			return fmt.Errorf("video content type must be exactly %s", normalizedType)
		}
	}
	if normalizedType == "input_file" && !openAIResponsesPartIsVideo(part) {
		return errors.New("video input cannot be combined with a non-video input_file on the Chat compatibility route")
	}
	return nil
}

func validateOpenAIResponsesVideoChatTools(tools gjson.Result) error {
	if !tools.IsArray() {
		return nil
	}
	for _, tool := range tools.Array() {
		if tool.Type == gjson.String && strings.TrimSpace(tool.String()) != "" {
			continue
		}
		toolType := strings.TrimSpace(tool.Get("type").String())
		switch toolType {
		case "function", "custom", "tool_search", "x_search":
		case "namespace":
			children := tool.Get("tools")
			if !children.IsArray() {
				children = tool.Get("children")
			}
			for _, child := range children.Array() {
				if strings.TrimSpace(child.Get("type").String()) != "function" {
					return errors.New("video input cannot be combined with a non-function namespace tool on the Chat compatibility route")
				}
			}
		default:
			return fmt.Errorf("video input cannot be combined with Responses tool type %q on the Chat compatibility route", toolType)
		}
	}
	return nil
}

// PrepareOpenAIChatVideoBridgeRequestBody reserves heap before an enabled
// Chat/Responses request body is copied into memory. Known sizes are admitted
// once; chunked or under-declared bodies extend the reservation ahead of reads.
func (s *OpenAIGatewayService) PrepareOpenAIChatVideoBridgeRequestBody(
	ctx context.Context,
	c *gin.Context,
	reservationHint int64,
) error {
	if s == nil || s.chatVideoBridge == nil || c == nil || c.Request == nil || c.Request.Body == nil {
		return nil
	}
	if openAIChatVideoBridgeIngressProtocol(c) == "" {
		return nil
	}
	session, err := s.openAIChatVideoBridgeSession(ctx, c)
	if err != nil || session == nil {
		return err
	}
	reader := &openAIChatVideoBridgeBodyReader{
		ctx:               ctx,
		source:            c.Request.Body,
		session:           session,
		reservationFactor: openAIChatVideoBridgeJSONBodyFactor,
	}
	// Both handlers can transiently hold one additional request-sized buffer:
	// Chat while normalizing tools/reasoning, and Responses while rewriting or
	// converting the request before account selection.
	if reservationHint > 0 {
		if err := reader.reserve(reservationHint); err != nil {
			return err
		}
	}
	c.Request.Body = reader
	return nil
}

type openAIChatVideoBridgeBodyReader struct {
	ctx               context.Context
	source            io.ReadCloser
	session           *OpenAIChatVideoBridgeSession
	credit            int64
	reservationFactor int64
}

func (r *openAIChatVideoBridgeBodyReader) Read(buffer []byte) (int, error) {
	if r == nil || r.source == nil {
		return 0, io.EOF
	}
	if len(buffer) > 0 && int64(len(buffer)) > r.credit {
		reserveBytes := openAIChatVideoBridgeBodyChunk
		if needed := int64(len(buffer)) - r.credit; needed > reserveBytes {
			reserveBytes = needed
		}
		if err := r.reserve(reserveBytes); err != nil {
			return 0, err
		}
	}
	read, err := r.source.Read(buffer)
	if read > 0 {
		r.credit -= int64(read)
	}
	return read, err
}

func (r *openAIChatVideoBridgeBodyReader) Close() error {
	if r == nil || r.source == nil {
		return nil
	}
	return r.source.Close()
}

func (r *openAIChatVideoBridgeBodyReader) reserve(size int64) error {
	if size <= 0 || r == nil || r.session == nil {
		return nil
	}
	if r.credit > math.MaxInt64-size {
		return newOpenAIChatVideoBridgeError(
			http.StatusRequestEntityTooLarge,
			"invalid_request_error",
			"Request body is too large to reserve safely",
			errors.New("media bridge body reservation overflow"),
		)
	}
	factor := r.reservationFactor
	if factor < 1 {
		factor = 1
	}
	if size > math.MaxInt64/factor {
		return newOpenAIChatVideoBridgeError(
			http.StatusRequestEntityTooLarge,
			"invalid_request_error",
			"Request body is too large to reserve safely",
			errors.New("media bridge body working-set reservation overflow"),
		)
	}
	if err := r.session.reserveRequestBody(r.ctx, size*factor); err != nil {
		return err
	}
	r.credit += size
	return nil
}

func (s *OpenAIGatewayService) openAIChatVideoBridgeSession(
	ctx context.Context,
	c *gin.Context,
) (*OpenAIChatVideoBridgeSession, error) {
	if s == nil || s.chatVideoBridge == nil || c == nil {
		return nil, nil
	}
	var state *openAIChatVideoBridgeGinState
	if existing, ok := c.Get(openAIChatVideoBridgeGinSessionKey); ok {
		state, _ = existing.(*openAIChatVideoBridgeGinState)
	}
	if state == nil {
		requestCtx := ctx
		if c.Request != nil {
			requestCtx = c.Request.Context()
		}
		stableKey, _ := requestCtx.Value(ctxkey.ClientRequestID).(string)
		if strings.TrimSpace(stableKey) == "" {
			stableKey, _ = requestCtx.Value(ctxkey.RequestID).(string)
		}
		tenantID, _ := requestCtx.Value(ctxkey.UserID).(int64)
		session, err := s.chatVideoBridge.NewSession(requestCtx, OpenAIChatVideoBridgeRequest{
			TenantID:         tenantID,
			StableKey:        stableKey,
			IngressProtocol:  openAIChatVideoBridgeIngressProtocol(c),
			UpstreamProtocol: MediaBridgeProtocolOpenAIChatCompletions,
		})
		if err != nil {
			return nil, err
		}
		state = &openAIChatVideoBridgeGinState{session: session}
		c.Set(openAIChatVideoBridgeGinSessionKey, state)
	}
	return state.session, nil
}

// CleanupOpenAIChatVideoBridgeSession is called when the HTTP handler has
// finished draining/closing the final upstream response. It deliberately does
// not follow request-context cancellation: CC streaming may keep draining an
// upstream after the client disconnects, and deleting a zero-delay object
// before that drain completes can break the upstream's URL fetch.
func (s *OpenAIGatewayService) CleanupOpenAIChatVideoBridgeSession(c *gin.Context) {
	if s == nil || c == nil {
		return
	}
	existing, ok := c.Get(openAIChatVideoBridgeGinSessionKey)
	if !ok {
		return
	}
	state, _ := existing.(*openAIChatVideoBridgeGinState)
	if state == nil || state.session == nil {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), openAIChatVideoBridgeCleanupTimeout)
	defer cancel()
	if err := state.session.Cleanup(cleanupCtx); err != nil {
		logger.L().Warn("openai chat video bridge request cleanup failed", zap.Error(err))
	}
}

func (b *OpenAIChatVideoBridge) NewSession(ctx context.Context, request OpenAIChatVideoBridgeRequest) (*OpenAIChatVideoBridgeSession, error) {
	if b == nil || b.store == nil || b.policies == nil {
		return nil, nil
	}
	policy, err := b.policies.SnapshotOpenAIChatVideoBridgePolicy(ctx, request)
	if err != nil {
		return nil, newOpenAIChatVideoBridgeError(http.StatusServiceUnavailable, "temporary_media_error", "Temporary video storage policy is unavailable", err)
	}
	normalized, err := normalizeOpenAIChatVideoBridgePolicy(policy)
	if err != nil {
		return nil, newOpenAIChatVideoBridgeError(http.StatusServiceUnavailable, "temporary_media_error", "Temporary video storage policy is invalid", err)
	}
	if !openAIChatVideoBridgePolicyEnabled(normalized, request) {
		return nil, nil
	}
	store := b.store
	relativePrefix := normalized.objectPrefix
	if b.storeResolver != nil {
		// Ordinary text and URL requests must not depend on R2 health. The first
		// eligible Data URI resolves and pins the dynamic store instead.
		store = nil
		relativePrefix = ""
	}
	return &OpenAIChatVideoBridgeSession{
		bridge:         b,
		store:          store,
		policy:         normalized,
		relativePrefix: relativePrefix,
		tenantID:       request.TenantID,
		cache:          make(map[openAIChatVideoCacheKey]openAIChatVideoCachedObject),
	}, nil
}

type normalizedOpenAIChatVideoBridgePolicy struct {
	mode              string
	canaryPercent     int
	ingressProtocols  map[string]struct{}
	upstreamProtocols map[string]struct{}
	models            map[string]struct{}
	accountIDs        map[int64]struct{}
	allowMP4          bool
	maxVideoBytes     int64
	maxRequestBytes   int64
	maxVideos         int64
	signedURLTTL      time.Duration
	deleteDelay       time.Duration
	deduplicate       bool
	objectPrefix      string
	capacity          MediaBridgeCapacitySettings
	uploadTimeout     time.Duration
}

func normalizeOpenAIChatVideoBridgePolicy(policy OpenAIChatVideoBridgePolicy) (normalizedOpenAIChatVideoBridgePolicy, error) {
	normalized := normalizedOpenAIChatVideoBridgePolicy{
		mode:              strings.ToLower(strings.TrimSpace(policy.Mode)),
		canaryPercent:     policy.CanaryPercent,
		ingressProtocols:  normalizeOpenAIChatVideoStringSet(policy.IngressProtocols),
		upstreamProtocols: normalizeOpenAIChatVideoStringSet(policy.UpstreamProtocols),
		models:            normalizeOpenAIChatVideoStringSet(policy.Models),
		accountIDs:        make(map[int64]struct{}, len(policy.AccountIDs)),
		maxVideoBytes:     policy.MaxVideoBytes,
		maxRequestBytes:   policy.MaxRequestBytes,
		maxVideos:         policy.MaxVideos,
		signedURLTTL:      policy.SignedURLTTL,
		deleteDelay:       policy.RequestEndDeleteDelay,
		deduplicate:       policy.Deduplicate,
		objectPrefix:      strings.Trim(strings.TrimSpace(policy.ObjectPrefix), "/"),
		capacity:          policy.Capacity,
		uploadTimeout:     policy.UploadTimeout,
	}
	switch normalized.mode {
	case MediaBridgeModeOff, MediaBridgeModeObserve, MediaBridgeModeDrain, MediaBridgeModeOn:
	case MediaBridgeModeCanary:
		if normalized.canaryPercent <= 0 || normalized.canaryPercent > 100 {
			return normalized, errors.New("canary percent must be between 1 and 100")
		}
	default:
		return normalized, errors.New("unsupported media bridge mode")
	}
	if normalized.maxVideoBytes < 0 || normalized.maxRequestBytes < 0 || normalized.maxVideos < 0 {
		return normalized, errors.New("media bridge file limits cannot be negative")
	}
	for _, accountID := range policy.AccountIDs {
		if accountID <= 0 {
			return normalized, errors.New("media bridge account IDs must be positive")
		}
		normalized.accountIDs[accountID] = struct{}{}
	}
	for _, value := range policy.AllowedMIMETypes {
		mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(value))
		if err != nil {
			return normalized, fmt.Errorf("invalid allowed media type: %w", err)
		}
		if strings.EqualFold(mediaType, openAIChatVideoBridgeContentType) {
			normalized.allowMP4 = true
		}
	}
	if normalized.mode != MediaBridgeModeOn && normalized.mode != MediaBridgeModeCanary {
		return normalized, nil
	}
	if normalized.signedURLTTL <= 0 {
		return normalized, errors.New("signed URL TTL must be positive")
	}
	if normalized.deleteDelay < 0 {
		return normalized, errors.New("request-end delete delay cannot be negative")
	}
	if normalized.uploadTimeout <= 0 {
		return normalized, errors.New("R2 upload timeout must be positive")
	}
	if !validOpenAIChatVideoRelativePrefix(normalized.objectPrefix) {
		return normalized, errors.New("invalid media bridge relative object prefix")
	}
	return normalized, nil
}

func validOpenAIChatVideoRelativePrefix(prefix string) bool {
	if len(prefix) > 192 || strings.Contains(prefix, "\\") {
		return false
	}
	if prefix == "" {
		return true
	}
	if path.Clean(prefix) != prefix {
		return false
	}
	for _, segment := range strings.Split(prefix, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func normalizeOpenAIChatVideoStringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value = strings.ToLower(strings.TrimSpace(value)); value != "" {
			result[value] = struct{}{}
		}
	}
	return result
}

func openAIChatVideoBridgePolicyEnabled(policy normalizedOpenAIChatVideoBridgePolicy, request OpenAIChatVideoBridgeRequest) bool {
	if policy.mode != MediaBridgeModeOn && policy.mode != MediaBridgeModeCanary {
		return false
	}
	if len(policy.ingressProtocols) > 0 {
		if _, ok := policy.ingressProtocols[strings.ToLower(strings.TrimSpace(request.IngressProtocol))]; !ok {
			return false
		}
	}
	if len(policy.upstreamProtocols) > 0 {
		if _, ok := policy.upstreamProtocols[strings.ToLower(strings.TrimSpace(request.UpstreamProtocol))]; !ok {
			return false
		}
	}
	if policy.mode != MediaBridgeModeCanary {
		return true
	}
	key := strings.TrimSpace(request.StableKey)
	if key == "" {
		key = fmt.Sprintf("tenant:%d", request.TenantID)
	}
	digest := sha256.Sum256([]byte(key))
	return int(binary.BigEndian.Uint64(digest[:8])%100) < policy.canaryPercent
}

func openAIChatVideoBridgeIngressProtocol(c *gin.Context) string {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return ""
	}
	requestPath := strings.ToLower(strings.TrimRight(strings.TrimSpace(c.Request.URL.Path), "/"))
	switch {
	case strings.HasSuffix(requestPath, "/chat/completions"):
		return MediaBridgeIngressOpenAIChatCompletions
	case strings.HasSuffix(requestPath, "/responses"):
		return MediaBridgeIngressOpenAIResponses
	default:
		return ""
	}
}

func openAIChatVideoBridgeBodyHasVideo(body []byte, ingressProtocol string) bool {
	switch strings.ToLower(strings.TrimSpace(ingressProtocol)) {
	case MediaBridgeIngressOpenAIChatCompletions:
		return openAIChatVideoMessagesHavePartType(body, "video_url")
	case MediaBridgeIngressOpenAIResponses:
		return openAIResponsesBodyHasVideo(body)
	default:
		return false
	}
}

func openAIChatVideoMessagesHavePartType(body []byte, expectedType string) bool {
	messages := gjson.GetBytes(body, "messages")
	if messages.IsArray() {
		for _, message := range messages.Array() {
			content := message.Get("content")
			if !content.IsArray() {
				continue
			}
			for _, part := range content.Array() {
				partType := strings.ToLower(strings.TrimSpace(part.Get("type").String()))
				if partType == expectedType {
					return true
				}
			}
		}
	}
	return false
}

func openAIResponsesBodyHasVideo(body []byte) bool {
	input := gjson.GetBytes(body, "input")
	if input.IsArray() {
		for _, item := range input.Array() {
			if openAIResponsesPartIsVideo(item) {
				return true
			}
			itemType := strings.ToLower(strings.TrimSpace(item.Get("type").String()))
			if itemType != "" && itemType != "message" {
				continue
			}
			content := item.Get("content")
			if !content.IsArray() {
				continue
			}
			for _, part := range content.Array() {
				if openAIResponsesPartIsVideo(part) {
					return true
				}
			}
		}
	}
	return false
}

func openAIResponsesPartIsVideo(part gjson.Result) bool {
	partType := strings.ToLower(strings.TrimSpace(part.Get("type").String()))
	if partType == "input_video" || partType == "video_url" {
		return true
	}
	if partType != "input_file" {
		return false
	}
	declaredMP4 := openAIResponsesPartDeclaresMP4(part)
	filenameMP4 := openAIChatVideoHasMP4PathSuffix(openAIChatVideoSmallJSONString(part.Get("filename")))
	fileData := part.Get("file_data")
	if openAIChatVideoRawJSONStringHasPrefix(fileData, "data:video/mp4") {
		return true
	}
	if openAIChatVideoRawJSONStringNonEmpty(fileData) &&
		!openAIChatVideoRawJSONStringHasPrefix(fileData, "data:") &&
		(declaredMP4 || filenameMP4) {
		return true
	}
	fileURLResult := part.Get("file_url")
	if fileURLResult.IsObject() {
		fileURLResult = fileURLResult.Get("url")
	}
	if openAIChatVideoRawJSONStringHasPrefix(fileURLResult, "data:video/mp4") {
		return true
	}
	if !openAIChatVideoRawJSONStringNonEmpty(fileURLResult) {
		return false
	}
	if declaredMP4 || filenameMP4 {
		return true
	}
	return openAIChatVideoHasMP4PathSuffix(openAIChatVideoSmallJSONString(fileURLResult))
}

func openAIResponsesPartDeclaresMP4(part gjson.Result) bool {
	for _, field := range []string{"mime_type", "media_type", "content_type"} {
		if strings.EqualFold(strings.TrimSpace(part.Get(field).String()), openAIChatVideoBridgeContentType) {
			return true
		}
	}
	return false
}

func openAIChatVideoRawJSONStringHasPrefix(result gjson.Result, prefix string) bool {
	raw := result.Raw
	if result.Type != gjson.String || len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return false
	}
	return openAIChatVideoJSONStringHasASCIIPrefix(raw[1:len(raw)-1], prefix)
}

func openAIChatVideoRawJSONStringNonEmpty(result gjson.Result) bool {
	return result.Type == gjson.String && len(result.Raw) > 2
}

func openAIChatVideoSmallJSONString(result gjson.Result) string {
	if result.IsObject() {
		result = result.Get("url")
	}
	if result.Type != gjson.String || len(result.Raw) > 8192 {
		return ""
	}
	return strings.TrimSpace(result.String())
}

func openAIChatVideoHasMP4PathSuffix(value string) bool {
	if query := strings.IndexAny(value, "?#"); query >= 0 {
		value = value[:query]
	}
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(value)), ".mp4")
}

func (p normalizedOpenAIChatVideoBridgePolicy) eligible(account *Account, upstreamModel string) bool {
	// Media Bridge only covers Chat/Responses-capable account routes. Accounts
	// pinned to Anthropic protocol retain their existing behavior unchanged.
	if account == nil || account.IsAnthropicProtocol() {
		return false
	}
	if len(p.models) > 0 {
		if _, ok := p.models[strings.ToLower(strings.TrimSpace(upstreamModel))]; !ok {
			return false
		}
	}
	if len(p.accountIDs) == 0 {
		return true
	}
	_, ok := p.accountIDs[account.ID]
	return ok
}

type openAIChatVideoBridgeContextKey struct{}

func WithOpenAIChatVideoBridgeSession(ctx context.Context, session *OpenAIChatVideoBridgeSession) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if session == nil {
		return ctx
	}
	return context.WithValue(ctx, openAIChatVideoBridgeContextKey{}, session)
}

func openAIChatVideoBridgeSessionFromContext(ctx context.Context) *OpenAIChatVideoBridgeSession {
	if ctx == nil {
		return nil
	}
	session, _ := ctx.Value(openAIChatVideoBridgeContextKey{}).(*OpenAIChatVideoBridgeSession)
	return session
}

type openAIChatVideoCacheKey struct {
	digest     [sha256.Size]byte
	occurrence string
}

type openAIChatVideoCachedObject struct {
	key       string
	url       string
	expiresAt time.Time
}

type OpenAIChatVideoBridgeSession struct {
	mu             sync.Mutex
	bridge         *OpenAIChatVideoBridge
	store          InlineMediaStore
	policy         normalizedOpenAIChatVideoBridgePolicy
	relativePrefix string
	tenantID       int64
	cache          map[openAIChatVideoCacheKey]openAIChatVideoCachedObject
	objectKeys     []string
	bodyLeases     []*MediaBridgeCapacityLease
	uploadedBytes  int64
	uploadedVideos int64
	closed         bool
}

func (s *OpenAIChatVideoBridgeSession) reserveRequestBody(ctx context.Context, size int64) error {
	if s == nil || s.bridge == nil || s.bridge.bodyCapacity == nil || size <= 0 {
		return nil
	}
	// Body admission intentionally uses only the administrator's wait budget.
	// Upload request/byte/bandwidth limits are enforced later against actual
	// decoded MP4 bytes and must not throttle ordinary text on the same ingress.
	settings := MediaBridgeCapacitySettings{
		AdmissionWaitMS:     s.policy.capacity.AdmissionWaitMS,
		DefaultTenantWeight: 1,
	}
	lease, err := s.bridge.bodyCapacity.Acquire(ctx, settings, s.tenantID, size)
	if err != nil {
		return openAIChatVideoBridgeCapacityError(
			err,
			"Temporary video bridge request-body capacity is busy",
			"Temporary video bridge request-body admission is unavailable",
		)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		lease.Release()
		return newOpenAIChatVideoBridgeError(
			http.StatusServiceUnavailable,
			"temporary_media_error",
			"Temporary video storage is unavailable",
			errors.New("chat video bridge session is closed"),
		)
	}
	s.bodyLeases = append(s.bodyLeases, lease)
	s.mu.Unlock()
	return nil
}

// MaterializeOpenAIChatVideoDataURLs runs at the final raw Chat Completions
// egress. It changes only messages[i].content[j].video_url.url values whose
// value is an MP4 base64 data URI. Every other JSON field remains untouched.
func MaterializeOpenAIChatVideoDataURLs(ctx context.Context, account *Account, upstreamModel string, body []byte) ([]byte, bool, error) {
	session := openAIChatVideoBridgeSessionFromContext(ctx)
	if session == nil || !session.policy.eligible(account, upstreamModel) {
		return body, false, nil
	}
	return session.materialize(ctx, body)
}

type openAIChatVideoReplacement struct {
	jsonStart  int
	jsonEnd    int
	encodedURL []byte
}

func (s *OpenAIChatVideoBridgeSession) materialize(ctx context.Context, body []byte) ([]byte, bool, error) {
	if s == nil || s.bridge == nil {
		return body, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, false, newOpenAIChatVideoBridgeError(http.StatusServiceUnavailable, "temporary_media_error", "Temporary video storage is unavailable", errors.New("chat video bridge session is closed"))
	}

	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return body, false, nil
	}
	replacements := make([]openAIChatVideoReplacement, 0)
	for messageIndex, message := range messages.Array() {
		content := message.Get("content")
		if !content.IsArray() {
			continue
		}
		for partIndex, part := range content.Array() {
			if part.Get("type").String() != "video_url" {
				continue
			}
			jsonPath := fmt.Sprintf("messages.%d.content.%d.video_url.url", messageIndex, partIndex)
			urlResult := gjson.GetBytes(body, jsonPath)
			dataURL, isDataURL, err := openAIChatVideoDataURLBytes(body, urlResult)
			if err != nil {
				return nil, false, newOpenAIChatVideoBridgeError(http.StatusBadRequest, "invalid_request_error", "video_url.url must use an unescaped data URI", err)
			}
			if !isDataURL {
				continue
			}
			object, err := s.materializeOne(ctx, dataURL, jsonPath)
			if err != nil {
				return nil, false, err
			}
			encodedURL, err := json.Marshal(object.url)
			if err != nil {
				return nil, false, newOpenAIChatVideoBridgeError(http.StatusInternalServerError, "api_error", "Failed to prepare video input", err)
			}
			replacements = append(replacements, openAIChatVideoReplacement{
				jsonStart:  urlResult.Index,
				jsonEnd:    urlResult.Index + len(urlResult.Raw),
				encodedURL: encodedURL,
			})
		}
	}
	if len(replacements) == 0 {
		return body, false, nil
	}
	updated, err := applyOpenAIChatVideoReplacements(body, replacements)
	if err != nil {
		return nil, false, err
	}
	return updated, true, nil
}

// materializeResponses rewrites the narrow Responses video compatibility
// surface directly in the original JSON bytes. input_file.file_data is moved
// to file_url after upload so the existing protocol adapter consumes the
// signed URL instead of treating it as bare base64.
func (s *OpenAIChatVideoBridgeSession) materializeResponses(ctx context.Context, body []byte) ([]byte, bool, error) {
	if s == nil || s.bridge == nil {
		return body, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, false, newOpenAIChatVideoBridgeError(http.StatusServiceUnavailable, "temporary_media_error", "Temporary video storage is unavailable", errors.New("chat video bridge session is closed"))
	}

	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return body, false, nil
	}
	replacements := make([]openAIChatVideoReplacement, 0)
	for itemIndex, item := range input.Array() {
		itemPath := fmt.Sprintf("input.%d", itemIndex)
		if err := s.collectResponsesVideoReplacements(ctx, body, itemPath, item, &replacements); err != nil {
			return nil, false, err
		}
		itemType := strings.TrimSpace(item.Get("type").String())
		if itemType != "" && itemType != "message" {
			continue
		}
		content := item.Get("content")
		if !content.IsArray() {
			continue
		}
		for partIndex, part := range content.Array() {
			partPath := fmt.Sprintf("%s.content.%d", itemPath, partIndex)
			if err := s.collectResponsesVideoReplacements(ctx, body, partPath, part, &replacements); err != nil {
				return nil, false, err
			}
		}
	}
	if len(replacements) == 0 {
		return body, false, nil
	}
	updated, err := applyOpenAIChatVideoReplacements(body, replacements)
	if err != nil {
		return nil, false, err
	}
	return updated, true, nil
}

func (s *OpenAIChatVideoBridgeSession) collectResponsesVideoReplacements(
	ctx context.Context,
	body []byte,
	partPath string,
	part gjson.Result,
	replacements *[]openAIChatVideoReplacement,
) error {
	switch strings.TrimSpace(part.Get("type").String()) {
	case "input_video", "video_url":
		urlPath := partPath + ".video_url"
		urlResult := gjson.GetBytes(body, urlPath)
		if urlResult.IsObject() {
			urlPath += ".url"
			urlResult = gjson.GetBytes(body, urlPath)
		}
		return s.collectResponsesDataURLReplacement(ctx, body, urlPath, urlResult, replacements)
	case "input_file":
		return s.collectResponsesInputFileReplacements(ctx, body, partPath, part, replacements)
	default:
		return nil
	}
}

func (s *OpenAIChatVideoBridgeSession) collectResponsesDataURLReplacement(
	ctx context.Context,
	body []byte,
	jsonPath string,
	urlResult gjson.Result,
	replacements *[]openAIChatVideoReplacement,
) error {
	dataURL, isDataURL, err := openAIChatVideoDataURLBytes(body, urlResult)
	if err != nil {
		return newOpenAIChatVideoBridgeError(http.StatusBadRequest, "invalid_request_error", "video URL must use an unescaped data URI", err)
	}
	if !isDataURL {
		return nil
	}
	object, err := s.materializeOne(ctx, dataURL, jsonPath)
	if err != nil {
		return err
	}
	encodedURL, err := json.Marshal(object.url)
	if err != nil {
		return newOpenAIChatVideoBridgeError(http.StatusInternalServerError, "api_error", "Failed to prepare video input", err)
	}
	*replacements = append(*replacements, openAIChatVideoReplacement{
		jsonStart:  urlResult.Index,
		jsonEnd:    urlResult.Index + len(urlResult.Raw),
		encodedURL: encodedURL,
	})
	return nil
}

func (s *OpenAIChatVideoBridgeSession) collectResponsesInputFileReplacements(
	ctx context.Context,
	body []byte,
	partPath string,
	part gjson.Result,
	replacements *[]openAIChatVideoReplacement,
) error {
	if !openAIResponsesPartIsVideo(part) {
		return nil
	}
	declaresMP4MIME := openAIResponsesPartDeclaresMP4(part)
	filenameMP4 := openAIChatVideoHasMP4PathSuffix(openAIChatVideoSmallJSONString(part.Get("filename")))
	declaredMP4 := declaresMP4MIME || filenameMP4
	fileDataPath := partPath + ".file_data"
	fileDataResult := gjson.GetBytes(body, fileDataPath)
	if fileDataResult.Type == gjson.String && len(fileDataResult.Raw) > 2 {
		value, err := openAIChatVideoJSONStringValueBytes(body, fileDataResult)
		if err != nil {
			return newOpenAIChatVideoBridgeError(http.StatusBadRequest, "invalid_request_error", "input_file.file_data must use unescaped MP4 base64", err)
		}
		var (
			object         openAIChatVideoCachedObject
			materializeErr error
			materialized   bool
		)
		switch {
		case asciiPrefixEqualFold(value, "data:video/mp4"):
			object, materializeErr = s.materializeOne(ctx, value, fileDataPath)
			materialized = true
		case !asciiPrefixEqualFold(value, "data:") && declaredMP4:
			object, materializeErr = s.materializeBareMP4Base64(ctx, value, fileDataPath)
			materialized = true
		}
		if materialized {
			if materializeErr != nil {
				return materializeErr
			}
			encodedURL, marshalErr := json.Marshal(object.url)
			if marshalErr != nil {
				return newOpenAIChatVideoBridgeError(http.StatusInternalServerError, "api_error", "Failed to prepare video input", marshalErr)
			}
			// Empty file_data so the adapter falls through to file_url.
			*replacements = append(*replacements, openAIChatVideoReplacement{
				jsonStart:  fileDataResult.Index,
				jsonEnd:    fileDataResult.Index + len(fileDataResult.Raw),
				encodedURL: []byte(`""`),
			})
			return appendOpenAIResponsesFileURLReplacement(body, partPath, encodedURL, !declaredMP4, replacements)
		}
	}

	fileURLPath := partPath + ".file_url"
	fileURLResult := gjson.GetBytes(body, fileURLPath)
	if fileURLResult.IsObject() {
		fileURLPath += ".url"
		fileURLResult = gjson.GetBytes(body, fileURLPath)
	}
	return s.collectResponsesDataURLReplacement(ctx, body, fileURLPath, fileURLResult, replacements)
}

func appendOpenAIResponsesFileURLReplacement(
	body []byte,
	partPath string,
	encodedURL []byte,
	ensureMP4Declaration bool,
	replacements *[]openAIChatVideoReplacement,
) error {
	insertion := make([]byte, 0, len(`,"file_url":`)+len(encodedURL)+len(`,"mime_type":"video/mp4"`))
	fileURLResult := gjson.GetBytes(body, partPath+".file_url")
	if fileURLResult.Exists() {
		if fileURLResult.IsObject() {
			nestedURL := gjson.GetBytes(body, partPath+".file_url.url")
			if nestedURL.Exists() {
				fileURLResult = nestedURL
			}
		}
		*replacements = append(*replacements, openAIChatVideoReplacement{
			jsonStart:  fileURLResult.Index,
			jsonEnd:    fileURLResult.Index + len(fileURLResult.Raw),
			encodedURL: encodedURL,
		})
	} else {
		insertion = append(insertion, `,"file_url":`...)
		insertion = append(insertion, encodedURL...)
	}
	if ensureMP4Declaration {
		declarationReplaced := false
		for _, field := range []string{"mime_type", "media_type", "content_type"} {
			result := gjson.GetBytes(body, partPath+"."+field)
			if !result.Exists() {
				continue
			}
			*replacements = append(*replacements, openAIChatVideoReplacement{
				jsonStart:  result.Index,
				jsonEnd:    result.Index + len(result.Raw),
				encodedURL: []byte(`"video/mp4"`),
			})
			declarationReplaced = true
			break
		}
		if !declarationReplaced {
			insertion = append(insertion, `,"mime_type":"video/mp4"`...)
		}
	}
	if len(insertion) == 0 {
		return nil
	}
	partResult := gjson.GetBytes(body, partPath)
	partEnd := partResult.Index + len(partResult.Raw)
	if !partResult.IsObject() || partEnd <= partResult.Index || partEnd > len(body) || body[partEnd-1] != '}' {
		return newOpenAIChatVideoBridgeError(http.StatusInternalServerError, "api_error", "Failed to prepare video input", errors.New("input_file JSON source range is invalid"))
	}
	*replacements = append(*replacements, openAIChatVideoReplacement{
		jsonStart:  partEnd - 1,
		jsonEnd:    partEnd - 1,
		encodedURL: insertion,
	})
	return nil
}

func openAIChatVideoJSONStringValueBytes(body []byte, result gjson.Result) ([]byte, error) {
	start := result.Index
	end := start + len(result.Raw)
	if result.Type != gjson.String || start < 0 || end > len(body) || end-start < 2 || body[start] != '"' || body[end-1] != '"' {
		return nil, errors.New("video JSON string source range is invalid")
	}
	value := body[start+1 : end-1]
	if bytes.IndexByte(value, '\\') >= 0 {
		return nil, errors.New("escaped video value is not supported")
	}
	return value, nil
}

func applyOpenAIChatVideoReplacements(body []byte, replacements []openAIChatVideoReplacement) ([]byte, error) {
	// Build the rewritten JSON once. Replacement ranges point directly into the
	// original body, so large data URIs are neither converted to strings nor
	// copied once per media part.
	sort.SliceStable(replacements, func(i, j int) bool {
		return replacements[i].jsonStart < replacements[j].jsonStart
	})
	finalLength := len(body)
	for _, replacement := range replacements {
		if replacement.jsonStart < 0 || replacement.jsonEnd < replacement.jsonStart || replacement.jsonEnd > len(body) {
			return nil, newOpenAIChatVideoBridgeError(http.StatusInternalServerError, "api_error", "Failed to prepare video input", errors.New("video URL JSON range is invalid"))
		}
		finalLength += len(replacement.encodedURL) - (replacement.jsonEnd - replacement.jsonStart)
	}
	if finalLength < 0 {
		return nil, newOpenAIChatVideoBridgeError(http.StatusInternalServerError, "api_error", "Failed to prepare video input", errors.New("rewritten video request length is invalid"))
	}
	updated := make([]byte, 0, finalLength)
	cursor := 0
	for _, replacement := range replacements {
		if replacement.jsonStart < cursor || replacement.jsonEnd > len(body) {
			return nil, newOpenAIChatVideoBridgeError(http.StatusInternalServerError, "api_error", "Failed to prepare video input", errors.New("overlapping video URL JSON ranges"))
		}
		updated = append(updated, body[cursor:replacement.jsonStart]...)
		updated = append(updated, replacement.encodedURL...)
		cursor = replacement.jsonEnd
	}
	updated = append(updated, body[cursor:]...)
	return updated, nil
}

func openAIChatVideoDataURLBytes(body []byte, result gjson.Result) ([]byte, bool, error) {
	if result.Type != gjson.String || result.Raw == "" {
		return nil, false, nil
	}
	start := result.Index
	end := start + len(result.Raw)
	if start < 0 || end > len(body) || end-start < 2 || body[start] != '"' || body[end-1] != '"' {
		return nil, false, errors.New("video URL JSON source range is invalid")
	}
	value := body[start+1 : end-1]
	if !asciiPrefixEqualFold(value, "data:") {
		if openAIChatVideoJSONStringHasASCIIPrefix(value, "data:") {
			return nil, true, errors.New("escaped data URI is not supported")
		}
		return nil, false, nil
	}
	if bytes.IndexByte(value, '\\') >= 0 {
		return nil, true, errors.New("escaped data URI is not supported")
	}
	return value, true, nil
}

func asciiPrefixEqualFold(value []byte, prefix string) bool {
	if len(value) < len(prefix) {
		return false
	}
	for index := range prefix {
		if !asciiByteEqualFold(value[index], prefix[index]) {
			return false
		}
	}
	return true
}

// openAIChatVideoJSONStringHasASCIIPrefix compares the decoded prefix of a
// JSON string without unquoting (and therefore copying) the full base64 value.
// The caller passes the bytes between the surrounding JSON quotes.
func openAIChatVideoJSONStringHasASCIIPrefix[T ~string | ~[]byte](value T, prefix string) bool {
	sourceIndex := 0
	for prefixIndex := range prefix {
		if sourceIndex >= len(value) {
			return false
		}
		decoded := value[sourceIndex]
		sourceIndex++
		if decoded == '\\' {
			if sourceIndex >= len(value) {
				return false
			}
			escaped := value[sourceIndex]
			sourceIndex++
			switch escaped {
			case '"', '\\', '/':
				decoded = escaped
			case 'b':
				decoded = '\b'
			case 'f':
				decoded = '\f'
			case 'n':
				decoded = '\n'
			case 'r':
				decoded = '\r'
			case 't':
				decoded = '\t'
			case 'u':
				if sourceIndex+4 > len(value) {
					return false
				}
				codePoint := 0
				for offset := 0; offset < 4; offset++ {
					nibble, ok := asciiHexNibble(value[sourceIndex+offset])
					if !ok {
						return false
					}
					codePoint = codePoint<<4 | int(nibble)
				}
				sourceIndex += 4
				if codePoint > 0x7f {
					return false
				}
				decoded = byte(codePoint)
			default:
				return false
			}
		}
		if !asciiByteEqualFold(decoded, prefix[prefixIndex]) {
			return false
		}
	}
	return true
}

func asciiByteEqualFold(left, right byte) bool {
	if left >= 'A' && left <= 'Z' {
		left += 'a' - 'A'
	}
	if right >= 'A' && right <= 'Z' {
		right += 'a' - 'A'
	}
	return left == right
}

func asciiHexNibble(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	default:
		return 0, false
	}
}

func (s *OpenAIChatVideoBridgeSession) materializeOne(ctx context.Context, dataURL []byte, occurrence string) (openAIChatVideoCachedObject, error) {
	cacheKey := s.openAIChatVideoCacheKey(sha256.Sum256(dataURL), occurrence)
	if object, ok, err := s.cachedOpenAIChatVideoObject(ctx, cacheKey); ok || err != nil {
		return object, err
	}
	payload, decodedBytes, err := parseStrictMP4Base64DataURL(dataURL)
	if err != nil {
		status := http.StatusBadRequest
		message := "video_url.url must be a valid data:video/mp4;base64 URI"
		if errors.Is(err, errOpenAIChatVideoUnsupportedMediaType) {
			status = http.StatusUnsupportedMediaType
			message = "Only data:video/mp4;base64 video input is supported"
		}
		return openAIChatVideoCachedObject{}, newOpenAIChatVideoBridgeError(status, "invalid_request_error", message, err)
	}
	return s.materializeMP4Payload(ctx, payload, decodedBytes, cacheKey)
}

// materializeBareMP4Base64 handles Responses input_file.file_data without
// allocating another request-sized slice solely to prepend a data URI header.
// Its digest matches the canonical data URI form so identical mixed inputs
// still share one temporary object.
func (s *OpenAIChatVideoBridgeSession) materializeBareMP4Base64(ctx context.Context, payload []byte, occurrence string) (openAIChatVideoCachedObject, error) {
	digestSource := sha256.New()
	digestSource.Write([]byte(openAIChatVideoMP4DataURLPrefix))
	digestSource.Write(payload)
	var digest [sha256.Size]byte
	digestSource.Sum(digest[:0])
	cacheKey := s.openAIChatVideoCacheKey(digest, occurrence)
	if object, ok, err := s.cachedOpenAIChatVideoObject(ctx, cacheKey); ok || err != nil {
		return object, err
	}
	decodedBytes, err := strictStandardBase64DecodedSize(payload)
	if err != nil {
		return openAIChatVideoCachedObject{}, newOpenAIChatVideoBridgeError(
			http.StatusBadRequest,
			"invalid_request_error",
			"input_file.file_data must contain valid padded MP4 base64",
			err,
		)
	}
	return s.materializeMP4Payload(ctx, payload, decodedBytes, cacheKey)
}

func (s *OpenAIChatVideoBridgeSession) openAIChatVideoCacheKey(digest [sha256.Size]byte, occurrence string) openAIChatVideoCacheKey {
	cacheKey := openAIChatVideoCacheKey{digest: digest}
	if !s.policy.deduplicate {
		cacheKey.occurrence = occurrence
	}
	return cacheKey
}

func (s *OpenAIChatVideoBridgeSession) cachedOpenAIChatVideoObject(
	ctx context.Context,
	cacheKey openAIChatVideoCacheKey,
) (openAIChatVideoCachedObject, bool, error) {
	object, ok := s.cache[cacheKey]
	if !ok {
		return openAIChatVideoCachedObject{}, false, nil
	}
	if time.Until(object.expiresAt) <= openAIChatVideoResignWindow(s.policy.signedURLTTL) {
		assetURL, err := s.store.PresignGet(ctx, object.key, s.policy.signedURLTTL)
		if err != nil {
			return openAIChatVideoCachedObject{}, true, newOpenAIChatVideoBridgeError(http.StatusServiceUnavailable, "temporary_media_error", "Failed to refresh temporary video URL", err)
		}
		if err := validateOpenAIChatVideoAssetURL(assetURL); err != nil {
			return openAIChatVideoCachedObject{}, true, newOpenAIChatVideoBridgeError(http.StatusServiceUnavailable, "temporary_media_error", "Temporary video storage returned an invalid asset URL", err)
		}
		object.url = assetURL
		object.expiresAt = time.Now().Add(s.policy.signedURLTTL)
		s.cache[cacheKey] = object
	}
	return object, true, nil
}

func (s *OpenAIChatVideoBridgeSession) materializeMP4Payload(
	ctx context.Context,
	payload []byte,
	decodedBytes int64,
	cacheKey openAIChatVideoCacheKey,
) (openAIChatVideoCachedObject, error) {
	if !s.policy.allowMP4 {
		return openAIChatVideoCachedObject{}, newOpenAIChatVideoBridgeError(http.StatusUnsupportedMediaType, "invalid_request_error", "MP4 video input is disabled by the current media policy", errOpenAIChatVideoUnsupportedMediaType)
	}
	if s.policy.maxVideoBytes > 0 && decodedBytes > s.policy.maxVideoBytes {
		return openAIChatVideoCachedObject{}, newOpenAIChatVideoBridgeError(http.StatusRequestEntityTooLarge, "invalid_request_error", "Decoded MP4 video exceeds the current per-file media limit", nil)
	}
	if s.policy.maxVideos > 0 && s.uploadedVideos >= s.policy.maxVideos {
		return openAIChatVideoCachedObject{}, newOpenAIChatVideoBridgeError(http.StatusRequestEntityTooLarge, "invalid_request_error", "Video input exceeds the current per-request file limit", nil)
	}
	if s.policy.maxRequestBytes > 0 && (s.uploadedBytes > s.policy.maxRequestBytes || decodedBytes > s.policy.maxRequestBytes-s.uploadedBytes) {
		return openAIChatVideoCachedObject{}, newOpenAIChatVideoBridgeError(http.StatusRequestEntityTooLarge, "invalid_request_error", "Video input exceeds the current per-request media limit", nil)
	}

	reader, err := strictMP4DecodedReader(payload, decodedBytes)
	if err != nil {
		return openAIChatVideoCachedObject{}, newOpenAIChatVideoBridgeError(http.StatusBadRequest, "invalid_request_error", "video input contains invalid MP4 base64 data", err)
	}
	if err := s.ensureStore(ctx); err != nil {
		return openAIChatVideoCachedObject{}, err
	}
	lease, err := s.bridge.capacity.Acquire(ctx, s.policy.capacity, s.tenantID, decodedBytes)
	if err != nil {
		return openAIChatVideoCachedObject{}, openAIChatVideoBridgeCapacityError(
			err,
			"Temporary video bridge capacity is busy",
			"Temporary video bridge admission is unavailable",
		)
	}
	defer lease.Release()
	uploadCtx, cancelUpload := context.WithTimeout(ctx, s.policy.uploadTimeout)
	defer cancelUpload()
	reader = lease.WrapReader(uploadCtx, reader)
	key, err := s.store.NewObjectKey(s.relativePrefix, "chat-video", ".mp4")
	if err != nil {
		return openAIChatVideoCachedObject{}, newOpenAIChatVideoBridgeError(http.StatusServiceUnavailable, "temporary_media_error", "Temporary video storage is unavailable", err)
	}
	// Track before Put: an ambiguous transport error may still have committed a
	// complete S3 object and must be covered by cleanup/lifecycle fallback.
	s.objectKeys = append(s.objectKeys, key)
	var permit *MediaBridgeR2Permit
	if s.bridge.r2Circuit != nil {
		permit, err = s.bridge.r2Circuit.Admission(uploadCtx)
		if err != nil {
			return openAIChatVideoCachedObject{}, openAIChatVideoBridgeCapacityError(
				err,
				"Temporary video storage is temporarily busy",
				"Temporary video storage admission is unavailable",
			)
		}
	}
	uploadStarted := time.Now()
	putErr := s.store.Put(uploadCtx, key, openAIChatVideoBridgeContentType, decodedBytes, reader)
	if s.bridge.r2Circuit != nil {
		s.bridge.r2Circuit.Observe(permit, time.Since(uploadStarted), putErr)
	}
	if putErr != nil {
		return openAIChatVideoCachedObject{}, newOpenAIChatVideoBridgeError(http.StatusServiceUnavailable, "temporary_media_error", "Failed to store temporary video input", putErr)
	}
	assetURL, err := s.store.PresignGet(uploadCtx, key, s.policy.signedURLTTL)
	if err != nil {
		return openAIChatVideoCachedObject{}, newOpenAIChatVideoBridgeError(http.StatusServiceUnavailable, "temporary_media_error", "Failed to sign temporary video input", err)
	}
	if err := validateOpenAIChatVideoAssetURL(assetURL); err != nil {
		return openAIChatVideoCachedObject{}, newOpenAIChatVideoBridgeError(http.StatusServiceUnavailable, "temporary_media_error", "Temporary video storage returned an invalid asset URL", err)
	}
	object := openAIChatVideoCachedObject{
		key:       key,
		url:       assetURL,
		expiresAt: time.Now().Add(s.policy.signedURLTTL),
	}
	s.cache[cacheKey] = object
	s.uploadedBytes += decodedBytes
	s.uploadedVideos++
	return object, nil
}

func openAIChatVideoResignWindow(ttl time.Duration) time.Duration {
	window := ttl / 10
	if window > 5*time.Minute {
		return 5 * time.Minute
	}
	if window < time.Second {
		return time.Second
	}
	return window
}

func (s *OpenAIChatVideoBridgeSession) ensureStore(ctx context.Context) error {
	if s.store != nil {
		return nil
	}
	if s.bridge == nil || s.bridge.storeResolver == nil {
		return newOpenAIChatVideoBridgeError(
			http.StatusServiceUnavailable,
			"temporary_media_error",
			"Temporary video storage is unavailable",
			ErrTemporaryMediaUnavailable,
		)
	}
	store, err := s.bridge.storeResolver.SnapshotStore(ctx)
	if err != nil || store == nil {
		return newOpenAIChatVideoBridgeError(
			http.StatusServiceUnavailable,
			"temporary_media_error",
			"Temporary video storage is unavailable",
			errors.Join(ErrTemporaryMediaUnavailable, err),
		)
	}
	s.store = store
	return nil
}

func openAIChatVideoBridgeCapacityError(err error, busyMessage, unavailableMessage string) *OpenAIChatVideoBridgeError {
	var capacityErr *MediaBridgeCapacityError
	if errors.As(err, &capacityErr) {
		if capacityErr.Reason == MediaBridgeCapacityReasonInflightBytes &&
			capacityErr.Limit > 0 && capacityErr.Requested > capacityErr.Limit {
			return newOpenAIChatVideoBridgeError(
				http.StatusRequestEntityTooLarge,
				"invalid_request_error",
				"Decoded MP4 video exceeds the current in-flight byte policy",
				err,
			)
		}
		if capacityErr.Reason == MediaBridgeCapacityReasonMemoryUnavailable ||
			capacityErr.Reason == MediaBridgeCapacityReasonR2CircuitOpen ||
			capacityErr.Reason == MediaBridgeCapacityReasonR2HalfOpenLimited {
			return newOpenAIChatVideoBridgeErrorWithRetry(
				http.StatusServiceUnavailable,
				"temporary_media_error",
				unavailableMessage,
				capacityErr.RetryAfter,
				err,
			)
		}
		return newOpenAIChatVideoBridgeErrorWithRetry(
			http.StatusTooManyRequests,
			"rate_limit_error",
			busyMessage,
			capacityErr.RetryAfter,
			err,
		)
	}
	return newOpenAIChatVideoBridgeError(
		http.StatusServiceUnavailable,
		"temporary_media_error",
		unavailableMessage,
		err,
	)
}

// Cleanup schedules deletion only after the request's configured grace period.
// A bucket lifecycle rule on ObjectPrefix remains the eventual-cleanup
// fallback for process restarts and failed deletion attempts.
func (s *OpenAIChatVideoBridgeSession) Cleanup(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	keys := append([]string(nil), s.objectKeys...)
	s.objectKeys = nil
	s.cache = nil
	bodyLeases := s.bodyLeases
	s.bodyLeases = nil
	bridge := s.bridge
	store := s.store
	delay := s.policy.deleteDelay
	s.mu.Unlock()
	for _, lease := range bodyLeases {
		lease.Release()
	}

	if len(keys) == 0 || bridge == nil || store == nil {
		return nil
	}
	if delay > 0 {
		bridge.schedule(delay, func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), openAIChatVideoBridgeCleanupTimeout)
			defer cancel()
			if err := deleteOpenAIChatVideoObjects(cleanupCtx, store, keys); err != nil {
				logger.L().Warn("openai chat video bridge delayed cleanup failed", zap.Error(err), zap.Int("object_count", len(keys)))
			}
		})
		return nil
	}
	return deleteOpenAIChatVideoObjects(ctx, store, keys)
}

func deleteOpenAIChatVideoObjects(ctx context.Context, store InlineMediaStore, keys []string) error {
	var joined error
	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if key = strings.TrimSpace(key); key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if err := store.Delete(ctx, key); err != nil {
			joined = errors.Join(joined, fmt.Errorf("delete temporary chat video: %w", err))
		}
	}
	return joined
}

func parseStrictMP4Base64DataURL(raw []byte) (payload []byte, decodedBytes int64, err error) {
	if !asciiPrefixEqualFold(raw, "data:") {
		return nil, 0, errors.New("not a data URI")
	}
	comma := bytes.IndexByte(raw[len("data:"):], ',')
	if comma < 0 {
		return nil, 0, errors.New("data URI is missing a payload")
	}
	comma += len("data:")
	header := raw[len("data:"):comma]
	payload = raw[comma+1:]
	if len(header) == 0 || len(header) > 256 || len(payload) == 0 {
		return nil, 0, errors.New("data URI header or payload is invalid")
	}
	parts := bytes.Split(header, []byte(";"))
	if len(parts) < 2 || !bytes.EqualFold(bytes.TrimSpace(parts[len(parts)-1]), []byte("base64")) {
		return nil, 0, errors.New("base64 marker must be the final data URI header token")
	}
	for _, part := range parts[1 : len(parts)-1] {
		if bytes.EqualFold(bytes.TrimSpace(part), []byte("base64")) {
			return nil, 0, errors.New("duplicate base64 marker")
		}
	}
	mediaType, parameters, parseErr := mime.ParseMediaType(string(bytes.Join(parts[:len(parts)-1], []byte(";"))))
	if parseErr != nil || !strings.EqualFold(mediaType, openAIChatVideoBridgeContentType) || len(parameters) != 0 {
		return nil, 0, errors.Join(errOpenAIChatVideoUnsupportedMediaType, parseErr)
	}
	decodedBytes, err = strictStandardBase64DecodedSize(payload)
	if err != nil {
		return nil, 0, err
	}
	return payload, decodedBytes, nil
}

func strictStandardBase64DecodedSize(payload []byte) (int64, error) {
	if len(payload) == 0 || len(payload)%4 != 0 {
		return 0, errors.New("base64 payload must use padded standard encoding")
	}
	padding := 0
	if payload[len(payload)-1] == '=' {
		padding++
	}
	if len(payload) > 1 && payload[len(payload)-2] == '=' {
		padding++
	}
	for index, char := range payload {
		valid := char >= 'A' && char <= 'Z' || char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '+' || char == '/'
		if valid {
			if index >= len(payload)-padding {
				return 0, errors.New("base64 padding is invalid")
			}
			continue
		}
		if char != '=' || index < len(payload)-padding {
			return 0, errors.New("base64 payload contains an invalid character")
		}
	}
	decoded := int64(len(payload)/4)*3 - int64(padding)
	if decoded <= 0 {
		return 0, errors.New("base64 payload is empty")
	}
	var finalQuantum [3]byte
	if _, err := base64.StdEncoding.Strict().Decode(finalQuantum[:], payload[len(payload)-4:]); err != nil {
		return 0, fmt.Errorf("base64 final quantum is not canonical: %w", err)
	}
	return decoded, nil
}

func strictMP4DecodedReader(payload []byte, decodedBytes int64) (io.Reader, error) {
	decoder := base64.NewDecoder(base64.StdEncoding.Strict(), bytes.NewReader(payload))
	prefixSize := int64(32)
	if decodedBytes < prefixSize {
		prefixSize = decodedBytes
	}
	prefix := make([]byte, int(prefixSize))
	if _, err := io.ReadFull(decoder, prefix); err != nil {
		return nil, fmt.Errorf("decode MP4 prefix: %w", err)
	}
	if err := validateMP4Prefix(prefix, decodedBytes); err != nil {
		return nil, err
	}
	return io.MultiReader(bytes.NewReader(prefix), decoder), nil
}

func validateMP4Prefix(prefix []byte, totalBytes int64) error {
	if len(prefix) < 16 || totalBytes < 16 {
		return errors.New("MP4 payload is too short")
	}
	boxSize := int64(binary.BigEndian.Uint32(prefix[:4]))
	if string(prefix[4:8]) != "ftyp" || boxSize < 16 || boxSize > totalBytes {
		return errors.New("MP4 payload must start with a valid ftyp box")
	}
	return nil
}

func validateOpenAIChatVideoAssetURL(rawURL string) error {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" || parsed.Opaque != "" {
		return errors.New("media object URL must be absolute HTTPS")
	}
	if parsed.User != nil || parsed.Fragment != "" || (parsed.Port() != "" && parsed.Port() != "443") {
		return errors.New("media object URL contains unsupported authority or fragment fields")
	}
	return nil
}

type OpenAIChatVideoBridgeError struct {
	StatusCode int
	Type       string
	Message    string
	RetryAfter time.Duration
	cause      error
}

func newOpenAIChatVideoBridgeError(statusCode int, errorType, message string, cause error) *OpenAIChatVideoBridgeError {
	return &OpenAIChatVideoBridgeError{StatusCode: statusCode, Type: errorType, Message: message, cause: cause}
}

func newOpenAIChatVideoBridgeErrorWithRetry(statusCode int, errorType, message string, retryAfter time.Duration, cause error) *OpenAIChatVideoBridgeError {
	return &OpenAIChatVideoBridgeError{
		StatusCode: statusCode,
		Type:       errorType,
		Message:    message,
		RetryAfter: retryAfter,
		cause:      cause,
	}
}

func (e *OpenAIChatVideoBridgeError) Error() string {
	if e == nil {
		return "chat video bridge error"
	}
	return e.Message
}

func (e *OpenAIChatVideoBridgeError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func AsOpenAIChatVideoBridgeError(err error) (*OpenAIChatVideoBridgeError, bool) {
	var bridgeErr *OpenAIChatVideoBridgeError
	if !errors.As(err, &bridgeErr) {
		return nil, false
	}
	return bridgeErr, true
}
