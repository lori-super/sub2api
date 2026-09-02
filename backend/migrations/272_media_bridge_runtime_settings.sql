-- worldcodes:release-mode=online-expand
SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '2min';

-- Runtime policy only. R2 access keys and secrets must remain in a dedicated
-- secret-backed runtime and are intentionally absent from this settings row.
INSERT INTO settings (key, value, updated_at)
VALUES (
    'media_bridge_settings',
    '{"version":1,"mode":"off","canary_percent":0,"scope":{"ingress_protocols":["openai_chat_completions","openai_responses"],"upstream_protocols":["openai_chat_completions"],"models":["kimi-k3"],"account_ids":[]},"capacity":{"max_inflight_requests":0,"max_inflight_decoded_bytes":0,"max_bandwidth_bytes_per_second":0,"burst_bytes":0,"admission_wait_ms":200,"default_tenant_weight":10,"tenant_overrides":[]},"protection":{"memory_soft_limit_percent":72,"memory_hard_limit_percent":82,"min_free_memory_bytes":0,"r2_error_rate_threshold_percent":5,"r2_latency_threshold_ms":0,"r2_window_seconds":60,"r2_open_seconds":30,"r2_half_open_probes":2,"r2_minimum_samples":20,"r2_upload_timeout_seconds":600},"file_policy":{"allowed_mime_types":["video/mp4"],"max_files_per_request":4,"max_single_decoded_bytes":134217728,"max_request_decoded_bytes":0,"deduplicate_within_request":true},"retention":{"signed_url_ttl_seconds":3600,"request_end_delete_delay_seconds":900},"storage":{"provider":"r2","endpoint":"","region":"auto","bucket":"","object_prefix":"media-bridge/"}}',
    NOW()
)
ON CONFLICT (key) DO NOTHING;
