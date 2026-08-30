# x5m5x 探测脚本与本地假上游

这组工具用于在不消耗真实额度的情况下复现 Token 价格、缓存价格、账本延迟、计费倍率和账本污染。假上游只监听 `127.0.0.1`，并且只接受以 `fake-`、`mock-` 或 `test-` 开头的 Bearer key。

## 安全边界

- 真实探测只允许访问 `https://api.x5m5x.com` 或 `https://us-api.x5m5x.com`。
- 访问真实 x5m5x 前必须显式设置 `X5M5X_CONFIRM_LIVE_PROBE=I_UNDERSTAND_THIS_COSTS_MONEY`。缺少或拼写不完全一致时，脚本会在网络请求前退出。
- 所有探测请求都拒绝 HTTP 重定向，避免 Bearer key 经跳转离开已校验的主机。
- HTTP 仅可用于 loopback；必须显式设置 `X5M5X_ALLOW_INSECURE_LOCAL=true`，并使用假 key。`sk-...` 等真实样式 key 会在发出请求前被拒绝。
- Token 与缓存探测都必须显式设置非空 `X5M5X_PROBE_MODELS`，不会隐式扫描全部模型。
- 两个探测脚本默认并发都是 `1`。只有明确设置 `X5M5X_PROBE_CONCURRENCY` 才会提高并发。
- `X5M5X_PROBE_MAX_REQUESTS` 是付费 `POST /v1/chat/completions` 的原子硬上限，默认为最小安全值 `4`。两种脚本都按每模型最多 4 次请求预检；例如显式选 2 个模型时必须将上限设为至少 `8`，否则在任何付费请求前退出。
- `X5M5X_PROBE_BUDGET` 和 `X5M5X_PROBE_RESERVATION` 只是费用的软限制（soft / best-effort）。它们会根据 `/v1/usage` 和并发预留停止后续分发，但上游只在请求后揭示真实费用，因此单个已发出的请求仍可能超过剩余预算，不能把该参数当成硬费用上限。
- 上游非 2xx 响应只记录 `status` 和安全 `code`，不打印响应正文，避免将上游返回的敏感信息带入日志。

真实探测前建议先只填一个模型、保持并发 `1`、硬请求上限 `4`，并将软预算设为你可以承受的观测值。

可先运行不访问网络的安全单元测试：

```powershell
node --test .\tools\x5m5x-probe-safety.test.mjs
```

## 可重复的本地 Token 探测

在仓库根目录打开第一个 PowerShell 窗口，启动假上游：

```powershell
$env:MOCK_X5M5X_PORT = '18085'
node .\tools\mock-x5m5x-upstream.mjs
```

看到下面一行说明服务已经就绪：

```text
MOCK_X5M5X_READY http://127.0.0.1:18085
```

在第二个 PowerShell 窗口运行 Token 探测：

```powershell
$env:X5M5X_API_BASE = 'http://127.0.0.1:18085'
$env:X5M5X_ALLOW_INSECURE_LOCAL = 'true'
$env:X5M5X_TOKEN_KEY = 'fake-probe-key'
$env:X5M5X_PROBE_MODELS = 'mock-alpha'
$env:X5M5X_PROBE_BUDGET = '0.01'
$env:X5M5X_PROBE_RESERVATION = '0.001'
$env:X5M5X_PROBE_CONCURRENCY = '1'
$env:X5M5X_PROBE_MAX_REQUESTS = '4'
node .\tools\x5m5x-token-probe.mjs
```

本地 loopback 模拟不需要、也不应设置 `X5M5X_CONFIRM_LIVE_PROBE`。

`mock-alpha` 的预设价格是输入 `$2 / 1M`、输出 `$8 / 1M`。输出最后一行以 `RESULT ` 开头，正常结果的 `input_per_million` 和 `output_per_million` 应分别接近 `2` 和 `8`。

## 可重复的本地缓存探测

沿用上面的假上游和环境变量，再设置本地报价页并运行：

```powershell
$env:X5M5X_PRICING_URL = 'http://127.0.0.1:18085/pricing/'
$env:X5M5X_PROBE_MODELS = 'mock-alpha'
$env:X5M5X_PROBE_BUDGET = '0.05'
$env:X5M5X_PROBE_RESERVATION = '0.015'
$env:X5M5X_CACHE_PREFIX_REPEATS = '64'
$env:X5M5X_PROBE_MAX_REQUESTS = '4'
node .\tools\x5m5x-cache-probe.mjs
```

预设缓存写入和读取价格分别是 `$2.5 / 1M` 与 `$0.2 / 1M`。缓存脚本会先重复同一个长前缀，第一次累计 `cache_creation_tokens`，第二次累计 `cache_read_tokens`。

## 真实上游的最小单模型运行

只在本地模拟通过、已确认会产生真实扣费后使用。下面以 Token 探测为例；将 `<one-exact-model-id>` 换成 `/v1/models` 中的一个精确 ID，不要填多个模型起步：

```powershell
$env:X5M5X_API_BASE = 'https://us-api.x5m5x.com'
$env:X5M5X_TOKEN_KEY = '<load-from-your-secret-manager>'
$env:X5M5X_PROBE_MODELS = '<one-exact-model-id>'
$env:X5M5X_PROBE_CONCURRENCY = '1'
$env:X5M5X_PROBE_MAX_REQUESTS = '4'
$env:X5M5X_PROBE_BUDGET = '0.01'
$env:X5M5X_PROBE_RESERVATION = '0.0025'
$env:X5M5X_CONFIRM_LIVE_PROBE = 'I_UNDERSTAND_THIS_COSTS_MONEY'
node .\tools\x5m5x-token-probe.mjs
```

`X5M5X_PROBE_MAX_REQUESTS=4` 是硬请求数上限，但 `0.01` 仍只是软费用预算。如果单次请求的真实价格超过剩余预算，已发出的该请求仍会扣费；脚本只能在账本可见后阻止后续请求。

## 修改价格与计费上下文

所有控制接口都只存在于 loopback 假上游。

修改模型四维价格：

```powershell
$body = @{
  model = 'mock-alpha'
  prices = @{
    input_per_million = 3
    output_per_million = 10
    cache_write_per_million = 3.75
    cache_read_per_million = 0.3
  }
} | ConvertTo-Json -Depth 4
Invoke-RestMethod -Method Post -Uri http://127.0.0.1:18085/_control/prices -ContentType application/json -Body $body
```

模拟计费倍率、固定请求费和账本延迟两次轮询：

```powershell
$body = @{
  request_multiplier = 1.2
  fixed_per_request = 0.00001
  usage_delay_polls = 2
} | ConvertTo-Json
Invoke-RestMethod -Method Post -Uri http://127.0.0.1:18085/_control/billing -ContentType application/json -Body $body
```

`GET /v1/sub2api/billing` 会返回当前计费上下文和全部模型价格。

## 模拟共享 Key 污染

下面的控制会在下一次 chat 请求计账时额外插入一条外部请求。探测器随后看到 `requests` 差值为 `2`，应将样本标记为 `ledger_request_delta_2`，而不是拿污染后的账本求价：

```powershell
$body = @{
  trigger = 'next_chat'
  model = 'mock-alpha'
  requests = 1
  input_tokens = 77
  output_tokens = 3
  actual_cost = 0.0002
} | ConvertTo-Json
Invoke-RestMethod -Method Post -Uri http://127.0.0.1:18085/_control/pollution -ContentType application/json -Body $body
node .\tools\x5m5x-token-probe.mjs
```

`trigger` 还支持 `next_usage` 和 `immediate`，便于测试 baseline 与外部累计账本。

## 重置与检查

完全恢复默认价格、计费上下文、缓存、累计账本和污染队列：

```powershell
Invoke-RestMethod -Method Post -Uri http://127.0.0.1:18085/_control/reset
```

可直接检查四维累计量和实际费用：

```powershell
$headers = @{ Authorization = 'Bearer fake-probe-key' }
Invoke-RestMethod -Uri http://127.0.0.1:18085/v1/usage -Headers $headers | ConvertTo-Json -Depth 6
```

结束测试后关闭假上游窗口，并清除当前 PowerShell 进程里的模拟变量，避免误用于真实探测：

```powershell
$probeVariables = @(
  'MOCK_X5M5X_PORT', 'X5M5X_API_BASE', 'X5M5X_PRICING_URL',
  'X5M5X_ALLOW_INSECURE_LOCAL', 'X5M5X_TOKEN_KEY', 'X5M5X_PROBE_MODELS',
  'X5M5X_PROBE_BUDGET', 'X5M5X_PROBE_RESERVATION', 'X5M5X_PROBE_CONCURRENCY',
  'X5M5X_PROBE_MAX_REQUESTS', 'X5M5X_CONFIRM_LIVE_PROBE',
  'X5M5X_CACHE_PREFIX_REPEATS'
)
$probeVariables | ForEach-Object { Remove-Item "Env:$_" -ErrorAction SilentlyContinue }
```
