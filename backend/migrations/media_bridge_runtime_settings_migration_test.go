package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMediaBridgeRuntimeSettingsMigrationSeedsDisabledNonSecretPolicy(t *testing.T) {
	content, err := FS.ReadFile("272_media_bridge_runtime_settings.sql")
	require.NoError(t, err)
	sql := strings.ToLower(string(content))

	require.Contains(t, sql, "-- worldcodes:release-mode=online-expand")
	require.Contains(t, sql, "set local lock_timeout")
	require.Contains(t, sql, "set local statement_timeout")
	require.Contains(t, sql, "'media_bridge_settings'")
	require.Contains(t, sql, `"mode":"off"`)
	require.Contains(t, sql, `"max_inflight_requests":0`)
	require.Contains(t, sql, `"r2_minimum_samples":20`)
	require.Contains(t, sql, `"r2_upload_timeout_seconds":600`)
	require.Contains(t, sql, `"ingress_protocols":["openai_chat_completions","openai_responses"]`)
	require.Contains(t, sql, `"upstream_protocols":["openai_chat_completions"]`)
	require.Contains(t, sql, `"models":["kimi-k3"]`)
	require.Contains(t, sql, `"signed_url_ttl_seconds":3600`)
	require.Contains(t, sql, `"request_end_delete_delay_seconds":900`)
	require.Contains(t, sql, `"provider":"r2"`)
	require.Contains(t, sql, "on conflict (key) do nothing")
	require.NotContains(t, sql, "access_key_id")
	require.NotContains(t, sql, "secret_access_key")
}
