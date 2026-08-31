ALTER TABLE upstream_price_monitor_config
    ADD COLUMN IF NOT EXISTS per_request_models TEXT[] NOT NULL DEFAULT ARRAY[
        'Auto-Model',
        'deepseek-v4-flash-0731',
        'deepseek-v4-pro-0813',
        'glm-5.1',
        'glm-5.2',
        'glm-5.3',
        'glm-5.3-flash',
        'gpt-5.6',
        'grok-4.6',
        'kimi-k2.6',
        'kimi-k2.7-code',
        'MiniMax-M2.7',
        'MiniMax-M2.7-highspeed',
        'MiniMax-M3'
    ]::TEXT[];

COMMENT ON COLUMN upstream_price_monitor_config.per_request_models IS
    '按次报价页独立监控范围；无需出现在按量账号的 /v1/models 中';
