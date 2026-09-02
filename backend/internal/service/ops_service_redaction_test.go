package service

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestIsSensitiveKey_TokenBudgetKeysNotRedacted(t *testing.T) {
	t.Parallel()

	for _, key := range []string{
		"max_tokens",
		"max_output_tokens",
		"max_input_tokens",
		"max_completion_tokens",
		"max_tokens_to_sample",
		"budget_tokens",
		"prompt_tokens",
		"completion_tokens",
		"input_tokens",
		"output_tokens",
		"total_tokens",
		"token_count",
	} {
		if isSensitiveKey(key) {
			t.Fatalf("expected key %q to NOT be treated as sensitive", key)
		}
	}

	for _, key := range []string{
		"authorization",
		"Authorization",
		"access_token",
		"refresh_token",
		"id_token",
		"session_token",
		"token",
		"client_secret",
		"private_key",
		"signature",
	} {
		if !isSensitiveKey(key) {
			t.Fatalf("expected key %q to be treated as sensitive", key)
		}
	}
}

func TestSanitizeAndTrimJSONPayload_PreservesTokenBudgetFields(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"model":"claude-3","max_tokens":123,"thinking":{"type":"enabled","budget_tokens":456},"access_token":"abc","messages":[{"role":"user","content":"hi"}]}`)
	out, _, _ := sanitizeAndTrimJSONPayload(raw, 10*1024)
	if out == "" {
		t.Fatalf("expected non-empty sanitized output")
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("unmarshal sanitized output: %v", err)
	}

	if got, ok := decoded["max_tokens"].(float64); !ok || got != 123 {
		t.Fatalf("expected max_tokens=123, got %#v", decoded["max_tokens"])
	}

	thinking, ok := decoded["thinking"].(map[string]any)
	if !ok || thinking == nil {
		t.Fatalf("expected thinking object to be preserved, got %#v", decoded["thinking"])
	}
	if got, ok := thinking["budget_tokens"].(float64); !ok || got != 456 {
		t.Fatalf("expected thinking.budget_tokens=456, got %#v", thinking["budget_tokens"])
	}

	if got := decoded["access_token"]; got != "[REDACTED]" {
		t.Fatalf("expected access_token to be redacted, got %#v", got)
	}
}

func TestShrinkToEssentials_IncludesThinking(t *testing.T) {
	t.Parallel()

	root := map[string]any{
		"model":      "claude-3",
		"max_tokens": 100,
		"thinking": map[string]any{
			"type":          "enabled",
			"budget_tokens": 200,
		},
		"messages": []any{
			map[string]any{"role": "user", "content": "first"},
			map[string]any{"role": "user", "content": "last"},
		},
	}

	out := shrinkToEssentials(root)
	if _, ok := out["thinking"]; !ok {
		t.Fatalf("expected thinking to be included in essentials: %#v", out)
	}
}

func TestSanitizeErrorBodyForStorageRedactsPresignedURLSecrets(t *testing.T) {
	t.Parallel()

	url := "https://media.example.test/object.mp4?X-Amz-Credential=credential-secret&X-Amz-Signature=signature-secret&X-Amz-Security-Token=token-secret"
	for _, raw := range []string{
		`{"error":{"message":"download failed: ` + url + `"}}`,
		"download failed: " + url,
	} {
		out, _ := sanitizeErrorBodyForStorage(raw, 10*1024)
		for _, secret := range []string{"credential-secret", "signature-secret", "token-secret"} {
			if strings.Contains(out, secret) {
				t.Fatalf("expected %q to be redacted from %q", secret, out)
			}
		}
		if !strings.Contains(out, "***") {
			t.Fatalf("expected redaction marker in %q", out)
		}
	}
}

func TestTruncateForLogRedactsPresignedURLSecretsBeforeTruncation(t *testing.T) {
	t.Parallel()

	encoded, err := json.Marshal(map[string]any{
		"error": "failed https://media.example.test/object?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=credential-secret&X-Amz-Signature=signature-secret&X-Amz-Security-Token=token-secret",
	})
	if err != nil {
		t.Fatalf("marshal test body: %v", err)
	}
	out := truncateForLog(encoded, 4096)
	for _, secret := range []string{"credential-secret", "signature-secret", "token-secret"} {
		if strings.Contains(out, secret) {
			t.Fatalf("expected %q to be redacted: %q", secret, out)
		}
	}
	for _, parameter := range []string{"X-Amz-Credential=***", "X-Amz-Signature=***", "X-Amz-Security-Token=***"} {
		if !strings.Contains(out, parameter) {
			t.Fatalf("expected %q in sanitized log body: %q", parameter, out)
		}
	}
}
