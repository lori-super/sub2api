import http from 'node:http'

const host = '127.0.0.1'
const port = Number(process.env.MOCK_X5M5X_PORT || '18085')
if (!Number.isInteger(port) || port < 1 || port > 65535) {
  throw new Error('MOCK_X5M5X_PORT must be an integer from 1 to 65535')
}

const DEFAULT_PRICES = {
  'mock-alpha': {
    input_per_million: 2,
    output_per_million: 8,
    cache_write_per_million: 2.5,
    cache_read_per_million: 0.2
  },
  'mock-beta': {
    input_per_million: 4,
    output_per_million: 12,
    cache_write_per_million: 5,
    cache_read_per_million: 0.4
  }
}

const DEFAULT_BILLING = {
  currency: 'USD',
  unit: 'per_million_tokens',
  request_multiplier: 1,
  fixed_per_request: 0,
  usage_delay_polls: 0
}

let prices
let billing
let stats
let pending
let pollution
let cachedSessions
let usagePolls

function copy(value) {
  return JSON.parse(JSON.stringify(value))
}

function reset() {
  prices = copy(DEFAULT_PRICES)
  billing = copy(DEFAULT_BILLING)
  stats = new Map()
  pending = []
  pollution = []
  cachedSessions = new Set()
  usagePolls = 0
}

reset()

function json(response, status, body) {
  response.writeHead(status, {
    'Content-Type': 'application/json; charset=utf-8',
    'Cache-Control': 'no-store'
  })
  response.end(JSON.stringify(body))
}

function text(response, status, body, contentType = 'text/plain; charset=utf-8') {
  response.writeHead(status, { 'Content-Type': contentType, 'Cache-Control': 'no-store' })
  response.end(body)
}

async function readJSON(request) {
  const chunks = []
  let size = 0
  for await (const chunk of request) {
    size += chunk.length
    if (size > 1_048_576) throw new Error('request body exceeds 1 MiB')
    chunks.push(chunk)
  }
  if (chunks.length === 0) return {}
  return JSON.parse(Buffer.concat(chunks).toString('utf8'))
}

function requireFakeKey(request, response) {
  const authorization = request.headers.authorization || ''
  if (!/^Bearer\s+(?:fake|mock|test)[-_][A-Za-z0-9._-]+$/i.test(authorization)) {
    json(response, 401, { error: { message: 'mock accepts only fake-/mock-/test- prefixed Bearer keys' } })
    return false
  }
  return true
}

function blankStat(model) {
  return {
    model,
    requests: 0,
    input_tokens: 0,
    output_tokens: 0,
    cache_creation_tokens: 0,
    cache_read_tokens: 0,
    actual_cost: 0
  }
}

function applyLedger(event) {
  const row = stats.get(event.model) || blankStat(event.model)
  row.requests += Number(event.requests || 0)
  row.input_tokens += Number(event.input_tokens || 0)
  row.output_tokens += Number(event.output_tokens || 0)
  row.cache_creation_tokens += Number(event.cache_creation_tokens || 0)
  row.cache_read_tokens += Number(event.cache_read_tokens || 0)
  row.actual_cost = Number((row.actual_cost + Number(event.actual_cost || 0)).toFixed(12))
  stats.set(event.model, row)
}

function flushUsagePoll() {
  usagePolls++
  const remainingPollution = []
  for (const entry of pollution) {
    if (entry.trigger === 'next_usage') applyLedger(entry.event)
    else remainingPollution.push(entry)
  }
  pollution = remainingPollution
  const waiting = []
  for (const entry of pending) {
    entry.pollsRemaining--
    if (entry.pollsRemaining <= 0) applyLedger(entry.event)
    else waiting.push(entry)
  }
  pending = waiting
}

function usagePayload() {
  flushUsagePoll()
  const modelStats = [...stats.values()].sort((a, b) => a.model.localeCompare(b.model))
  const total = modelStats.reduce((result, row) => ({
    requests: result.requests + row.requests,
    input_tokens: result.input_tokens + row.input_tokens,
    output_tokens: result.output_tokens + row.output_tokens,
    cache_creation_tokens: result.cache_creation_tokens + row.cache_creation_tokens,
    cache_read_tokens: result.cache_read_tokens + row.cache_read_tokens,
    actual_cost: Number((result.actual_cost + row.actual_cost).toFixed(12))
  }), {
    requests: 0,
    input_tokens: 0,
    output_tokens: 0,
    cache_creation_tokens: 0,
    cache_read_tokens: 0,
    actual_cost: 0
  })
  return { usage: { total }, model_stats: modelStats, usage_polls: usagePolls }
}

function flattenContent(value) {
  if (typeof value === 'string') return value
  if (!Array.isArray(value)) return ''
  return value.map(part => typeof part === 'string' ? part : String(part?.text || '')).join(' ')
}

function inputText(messages) {
  return (Array.isArray(messages) ? messages : [])
    .map(message => flattenContent(message?.content))
    .join(' ')
}

function approximateTokens(value) {
  const words = value.trim().split(/\s+/).filter(Boolean).length
  return Math.max(1, words + Math.ceil(value.length / 48))
}

function calculateEvent(model, body, session) {
  const modelPrice = prices[model]
  const content = inputText(body.messages)
  const input = approximateTokens(content)
  const maxTokens = Math.max(1, Math.min(128, Number(body.max_tokens || 16)))
  const output = maxTokens <= 1 ? 1 : Math.min(maxTokens, content.includes('forty-word') ? 40 : maxTokens)

  let cacheCreation = 0
  let cacheRead = 0
  if (String(session || '').startsWith('pricing-cache-') && input >= 100) {
    const cacheKey = `${model}\0${session}\0${content}`
    const cacheTokens = Math.max(1, Math.floor(input * 0.8))
    if (cachedSessions.has(cacheKey)) cacheRead = cacheTokens
    else {
      cachedSessions.add(cacheKey)
      cacheCreation = cacheTokens
    }
  }

  const rawCost = (
    input * modelPrice.input_per_million +
    output * modelPrice.output_per_million +
    cacheCreation * modelPrice.cache_write_per_million +
    cacheRead * modelPrice.cache_read_per_million
  ) / 1_000_000 + Number(billing.fixed_per_request || 0)
  const actualCost = Number((rawCost * Number(billing.request_multiplier || 1)).toFixed(12))
  return {
    event: {
      model,
      requests: 1,
      input_tokens: input,
      output_tokens: output,
      cache_creation_tokens: cacheCreation,
      cache_read_tokens: cacheRead,
      actual_cost: actualCost
    },
    usage: {
      prompt_tokens: input,
      completion_tokens: output,
      total_tokens: input + output,
      prompt_tokens_details: { cached_tokens: cacheRead },
      cache_creation_tokens: cacheCreation,
      actual_cost: actualCost
    }
  }
}

function queueLedger(event) {
  const delay = Math.max(0, Math.floor(Number(billing.usage_delay_polls || 0)))
  if (delay === 0) applyLedger(event)
  else pending.push({ event, pollsRemaining: delay })
}

function applyNextChatPollution() {
  const remaining = []
  for (const entry of pollution) {
    if (entry.trigger === 'next_chat') applyLedger(entry.event)
    else remaining.push(entry)
  }
  pollution = remaining
}

function normalizePrice(candidate, current = {}) {
  const result = { ...current }
  for (const key of [
    'input_per_million',
    'output_per_million',
    'cache_write_per_million',
    'cache_read_per_million'
  ]) {
    if (candidate[key] === undefined) continue
    const value = Number(candidate[key])
    if (!Number.isFinite(value) || value < 0) throw new Error(`${key} must be a non-negative number`)
    result[key] = value
  }
  for (const key of ['input_per_million', 'output_per_million', 'cache_write_per_million', 'cache_read_per_million']) {
    if (!Number.isFinite(result[key])) throw new Error(`${key} is required`)
  }
  return result
}

async function handleControl(pathname, request, response) {
  if (request.method === 'GET') {
    if (pathname === '/_control/prices') return json(response, 200, { prices })
    if (pathname === '/_control/billing') return json(response, 200, { billing })
    if (pathname === '/_control/pollution') return json(response, 200, { queued: pollution, pending })
  }
  if (request.method !== 'POST') return json(response, 405, { error: 'method_not_allowed' })
  const body = await readJSON(request)

  if (pathname === '/_control/reset') {
    reset()
    return json(response, 200, { ok: true, prices, billing })
  }
  if (pathname === '/_control/prices') {
    const updates = body.models || (body.model ? { [body.model]: body.prices || body } : body)
    for (const [model, candidate] of Object.entries(updates)) {
      if (!model.trim()) throw new Error('model name must not be empty')
      prices[model] = normalizePrice(candidate, prices[model])
    }
    return json(response, 200, { ok: true, prices })
  }
  if (pathname === '/_control/billing') {
    const next = { ...billing, ...body }
    for (const key of ['request_multiplier', 'fixed_per_request', 'usage_delay_polls']) {
      const value = Number(next[key])
      if (!Number.isFinite(value) || value < 0) throw new Error(`${key} must be a non-negative number`)
      next[key] = value
    }
    billing = next
    return json(response, 200, { ok: true, billing })
  }
  if (pathname === '/_control/pollution') {
    const model = String(body.model || 'mock-alpha')
    const event = {
      model,
      requests: Number(body.requests ?? 1),
      input_tokens: Number(body.input_tokens || 0),
      output_tokens: Number(body.output_tokens || 0),
      cache_creation_tokens: Number(body.cache_creation_tokens || 0),
      cache_read_tokens: Number(body.cache_read_tokens || 0),
      actual_cost: Number(body.actual_cost || 0)
    }
    for (const [key, value] of Object.entries(event)) {
      if (key !== 'model' && (!Number.isFinite(value) || value < 0)) throw new Error(`${key} must be non-negative`)
    }
    const trigger = body.apply_immediately === true ? 'immediate' : String(body.trigger || 'next_chat')
    if (!['immediate', 'next_chat', 'next_usage'].includes(trigger)) {
      throw new Error('trigger must be immediate, next_chat, or next_usage')
    }
    if (trigger === 'immediate') applyLedger(event)
    else pollution.push({ trigger, event })
    return json(response, 200, { ok: true, queued: pollution.length, trigger, event })
  }
  return json(response, 404, { error: 'not_found' })
}

function pricingHTML() {
  const rows = Object.entries(prices).map(([model, price]) => `
    <tr class="token-model" data-model="${model}">
      <td data-label="模型">${model}</td>
      <td data-label="输入"><strong>¥${price.input_per_million}</strong></td>
      <td data-label="输出"><strong>¥${price.output_per_million}</strong></td>
    </tr>`).join('')
  return `<!doctype html><html><body><table>${rows}</table></body></html>`
}

const server = http.createServer(async (request, response) => {
  try {
    const url = new URL(request.url, `http://${host}:${port}`)
    const pathname = url.pathname.replace(/\/$/, '') || '/'

    if (pathname.startsWith('/_control/')) return await handleControl(pathname, request, response)
    if (request.method === 'GET' && (pathname === '/pricing' || pathname === '/pricing/index.html')) {
      return text(response, 200, pricingHTML(), 'text/html; charset=utf-8')
    }
    if (!requireFakeKey(request, response)) return

    if (request.method === 'GET' && pathname === '/v1/models') {
      return json(response, 200, {
        object: 'list',
        data: Object.keys(prices).sort().map(id => ({ id, object: 'model', owned_by: 'mock-x5m5x' }))
      })
    }
    if (request.method === 'GET' && pathname === '/v1/usage') {
      return json(response, 200, usagePayload())
    }
    if (request.method === 'GET' && pathname === '/v1/sub2api/billing') {
      return json(response, 200, { billing, models: prices })
    }
    if (request.method === 'POST' && pathname === '/v1/chat/completions') {
      const body = await readJSON(request)
      const model = String(body.model || '')
      if (!prices[model]) return json(response, 404, { error: { message: `unknown model: ${model}` } })
      const calculated = calculateEvent(model, body, request.headers['x-session-id'])
      queueLedger(calculated.event)
      applyNextChatPollution()
      return json(response, 200, {
        id: `chatcmpl-mock-${Date.now()}`,
        object: 'chat.completion',
        created: Math.floor(Date.now() / 1000),
        model,
        choices: [{ index: 0, message: { role: 'assistant', content: 'A' }, finish_reason: 'stop' }],
        usage: calculated.usage
      })
    }
    return json(response, 404, { error: { message: 'not_found' } })
  } catch (error) {
    return json(response, 400, { error: { message: error.message } })
  }
})

server.listen(port, host, () => {
  console.log(`MOCK_X5M5X_READY http://${host}:${port}`)
  console.log('Mock accepts only fake-/mock-/test- prefixed Bearer keys.')
})

for (const signal of ['SIGINT', 'SIGTERM']) {
  process.on(signal, () => server.close(() => process.exit(0)))
}
