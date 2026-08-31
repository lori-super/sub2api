package service

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	openAIAdaptiveChatFallbackTriedKey = "openai_adaptive_chat_fallback_tried"
	x5M5XSessionHeader                 = "X-Session-Id"
)

// isGenericOpenAIAdaptiveAccount reports whether a generic OpenAI-compatible
// API-key account should preserve the client's protocol at the upstream seam.
// Provider-specific CN accounts keep using credentials.api_protocol.
func isGenericOpenAIAdaptiveAccount(account *Account) bool {
	return account != nil &&
		account.Platform == PlatformOpenAI &&
		account.Type == AccountTypeAPIKey &&
		openai_compat.ShouldUseAdaptiveProtocol(account.Extra)
}

func adaptiveChatFallbackTried(c *gin.Context) bool {
	if c == nil {
		return false
	}
	value, ok := c.Get(openAIAdaptiveChatFallbackTriedKey)
	tried, _ := value.(bool)
	return ok && tried
}

func markAdaptiveChatFallbackTried(c *gin.Context) {
	if c != nil {
		c.Set(openAIAdaptiveChatFallbackTriedKey, true)
	}
}

// isOpenAIProtocolEndpointUnavailable only treats a route-level 404/405 as an
// unsupported protocol. A model-level 404 must not trigger a second request on
// another endpoint because the model mapping itself is invalid.
func isOpenAIProtocolEndpointUnavailable(status int, body []byte) bool {
	if status == http.StatusMethodNotAllowed {
		return true
	}
	if status != http.StatusNotFound {
		return false
	}

	for _, path := range []string{"error.type", "error.code", "type", "code"} {
		signal := strings.ToLower(strings.TrimSpace(gjson.GetBytes(body, path).String()))
		if strings.Contains(signal, "model_not_found") || strings.Contains(signal, "model-not-found") {
			return false
		}
	}
	message := strings.ToLower(strings.TrimSpace(firstNonEmpty(
		gjson.GetBytes(body, "error.message").String(),
		gjson.GetBytes(body, "message").String(),
	)))
	if strings.Contains(message, "model not found") ||
		(strings.Contains(message, "model") && strings.Contains(message, "not supported")) {
		return false
	}
	return true
}

func isX5M5XOpenAIAPIKeyAccount(account *Account) bool {
	if account == nil || account.Platform != PlatformOpenAI || account.Type != AccountTypeAPIKey {
		return false
	}
	parsed, err := url.Parse(strings.TrimSpace(account.GetOpenAIBaseURL()))
	if err != nil {
		return false
	}
	switch strings.ToLower(parsed.Hostname()) {
	case "api.x5m5x.com", "us-api.x5m5x.com":
		return true
	default:
		return false
	}
}

// x5M5XCacheIdentity derives a stable upstream affinity key from explicit
// client session signals or Responses prompt_cache_key. The raw tenant value is
// never forwarded: downstream API-key and upstream-account namespaces are mixed
// in first to prevent cross-user and cross-account cache/session collisions.
func x5M5XCacheIdentity(c *gin.Context, account *Account, body []byte) string {
	if !isX5M5XOpenAIAPIKeyAccount(account) {
		return ""
	}
	raw := explicitOpenAIRequestSessionID(c, body)
	if raw == "" {
		return ""
	}
	return isolateOpenAIUpstreamSessionID(getAPIKeyIDFromContext(c), account, raw)
}

func applyX5M5XCacheIdentity(headers http.Header, identity string) {
	if headers == nil {
		return
	}
	if identity == "" {
		headers.Del(x5M5XSessionHeader)
		return
	}
	headers.Set(x5M5XSessionHeader, identity)
}
