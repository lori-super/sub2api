import {
  AtomicBudget,
  AtomicRequestLimit,
  boundedConcurrency,
  positiveInteger,
  positiveNumber,
  requiredProbeModels,
  sanitizedFailure,
  sanitizedHTTPError,
  validatePricingURL,
  validateProbeTarget
} from './x5m5x-probe-safety.mjs'

const apiKey = process.env.X5M5X_TOKEN_KEY || ''
if (!apiKey) throw new Error('X5M5X_TOKEN_KEY is required')

const requestedModelList = requiredProbeModels()
const requestedModels = new Set(requestedModelList)
const maximumRequests = positiveInteger('X5M5X_PROBE_MAX_REQUESTS', '4')
const worstCaseRequests = requestedModelList.length * 4
if (maximumRequests < worstCaseRequests) {
  throw new Error(
    `X5M5X_PROBE_MAX_REQUESTS=${maximumRequests} is insufficient; ` +
    `${requestedModelList.length} model(s) require at least ${worstCaseRequests} request slots`
  )
}
const base = validateProbeTarget(process.env.X5M5X_API_BASE || 'https://us-api.x5m5x.com', apiKey)
const pricingURL = validatePricingURL(process.env.X5M5X_PRICING_URL || 'https://api.x5m5x.com/pricing/')
const budget = positiveNumber('X5M5X_PROBE_BUDGET', '0.05')
const concurrency = boundedConcurrency(1, 3)
const reservationAmount = positiveNumber(
  'X5M5X_PROBE_RESERVATION',
  String(Math.min(0.005, budget / Math.max(4, concurrency)))
)
const cachePrefixRepeats = Math.max(64, Math.min(400, Number(process.env.X5M5X_CACHE_PREFIX_REPEATS || '220')))

if (reservationAmount > budget) throw new Error('X5M5X_PROBE_RESERVATION must not exceed X5M5X_PROBE_BUDGET')

const headers = { Authorization: `Bearer ${apiKey}`, Accept: 'application/json' }
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms))

async function fetchTimeout(url, options = {}, timeoutMs = 120_000) {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), timeoutMs)
  try {
    return await fetch(url, { ...options, redirect: 'error', signal: controller.signal })
  } finally {
    clearTimeout(timer)
  }
}

async function getJSON(path, attempts = 5) {
  let lastFailure = { code: 'upstream_transport_error', status: 0 }
  for (let attempt = 1; attempt <= attempts; attempt++) {
    try {
      const response = await fetchTimeout(`${base}${path}`, { headers }, 45_000)
      if (!response.ok) {
        await response.body?.cancel()
        throw sanitizedHTTPError(response)
      }
      const text = await response.text()
      try {
        return JSON.parse(text)
      } catch {
        const error = new Error(`upstream_request_failed code=upstream_invalid_json status=${response.status}`)
        error.code = 'upstream_invalid_json'
        error.status = response.status
        throw error
      }
    } catch (error) {
      lastFailure = sanitizedFailure(error)
      if (attempt < attempts) await sleep(attempt * 1000)
    }
  }
  throw new Error(`upstream_request_failed code=${lastFailure.code} status=${lastFailure.status}`)
}

function stat(usage, model) {
  const row = Array.isArray(usage?.model_stats)
    ? usage.model_stats.find(item => item?.model === model)
    : null
  return {
    requests: Number(row?.requests || 0),
    input: Number(row?.input_tokens || 0),
    output: Number(row?.output_tokens || 0),
    write: Number(row?.cache_creation_tokens || 0),
    read: Number(row?.cache_read_tokens || 0),
    cost: Number(row?.actual_cost || 0)
  }
}

function delta(before, after) {
  return {
    requests: after.requests - before.requests,
    input: after.input - before.input,
    output: after.output - before.output,
    cache_write: after.write - before.write,
    cache_read: after.read - before.read,
    cost: Number((after.cost - before.cost).toFixed(12))
  }
}

function totalCost(usage) {
  return Number(usage?.usage?.total?.actual_cost || 0)
}

async function waitLedger(model, before, maxMs = 45_000) {
  const deadline = Date.now() + maxMs
  let current = before
  let lastUsage = null
  while (Date.now() < deadline) {
    await sleep(1500)
    const usage = await getJSON('/v1/usage')
    lastUsage = usage
    current = stat(usage, model)
    if (current.requests > before.requests) return { delta: delta(before, current), usage }
  }
  return { delta: delta(before, current), usage: lastUsage }
}

async function send(model, body, session) {
  const beforeUsage = await getJSON('/v1/usage')
  budgetGuard.observe(totalCost(beforeUsage))
  const reservation = budgetGuard.reserve(`${model}:${session}`)
  if (!reservation) return { dispatched: false, error: 'budget_exhausted', usage: null, delta: null }
  if (!requestGuard.acquire()) {
    budgetGuard.cancel(reservation)
    budgetGuard.halt()
    return { dispatched: false, error: 'request_limit_exhausted', usage: null, delta: null }
  }
  const before = stat(beforeUsage, model)
  let responseUsage = null
  let error = null
  try {
    const response = await fetchTimeout(`${base}/v1/chat/completions`, {
      method: 'POST',
      headers: {
        ...headers,
        'Content-Type': 'application/json',
        'X-Session-Id': session
      },
      body: JSON.stringify(body)
    })
    if (!response.ok) {
      await response.body?.cancel()
      error = { code: 'upstream_http_error', status: response.status }
    } else {
      const text = await response.text()
      try {
        responseUsage = JSON.parse(text).usage || null
      } catch {
        error = { code: 'upstream_invalid_json', status: response.status }
      }
    }
  } catch (caught) {
    error = sanitizedFailure(caught)
  }

  // Always inspect the ledger before any subsequent request.
  const observed = await waitLedger(model, before, error ? 15_000 : 45_000)
  if (observed.usage) budgetGuard.settle(reservation, totalCost(observed.usage))
  else {
    budgetGuard.cancel(reservation)
    budgetGuard.halt()
  }
  if (observed.delta.requests !== 1) {
    return {
      dispatched: true,
      error: error || `ledger_request_delta_${observed.delta.requests}`,
      usage: responseUsage,
      delta: observed.delta
    }
  }
  return { dispatched: true, error, usage: responseUsage, delta: observed.delta }
}

function priceFromResidual(row, inputPrice, outputPrice) {
  return row.cost - row.input * inputPrice / 1_000_000 - row.output * outputPrice / 1_000_000
}

function solveCache(rows, inputPrice, outputPrice) {
  const usable = rows.filter(row => row?.delta?.requests === 1 && !row.error)
  const observations = usable.map(row => ({
    write: row.delta.cache_write,
    read: row.delta.cache_read,
    residual: priceFromResidual(row.delta, inputPrice, outputPrice)
  }))
  let writePrice = null
  let readPrice = null

  for (const item of observations) {
    if (item.write > 0 && item.read === 0) writePrice = item.residual * 1_000_000 / item.write
    if (item.read > 0 && item.write === 0) readPrice = item.residual * 1_000_000 / item.read
  }
  if ((writePrice == null || readPrice == null) && observations.length >= 2) {
    for (let i = 0; i < observations.length - 1; i++) {
      for (let j = i + 1; j < observations.length; j++) {
        const a = observations[i]
        const b = observations[j]
        const determinant = a.write * b.read - b.write * a.read
        if (Math.abs(determinant) < 1e-9) continue
        writePrice = (a.residual * b.read - b.residual * a.read) * 1_000_000 / determinant
        readPrice = (a.write * b.residual - b.write * a.residual) * 1_000_000 / determinant
      }
    }
  }

  const normalize = value => Number.isFinite(value) && value >= -1e-8 ? Number(Math.max(0, value).toFixed(8)) : null
  return {
    cache_write_per_million: normalize(writePrice),
    cache_read_per_million: normalize(readPrice),
    observed_write_tokens: observations.reduce((sum, row) => sum + row.write, 0),
    observed_read_tokens: observations.reduce((sum, row) => sum + row.read, 0)
  }
}

function decodeText(html) {
  return html.replace(/<[^>]+>/g, '').replaceAll('&nbsp;', ' ').trim()
}

function parsePrice(html) {
  const value = decodeText(html).replace(/[\u00a5\uffe5,]/g, '').trim()
  return /^\d+(?:\.\d+)?$/.test(value) ? Number(value) : null
}

async function declaredPrices() {
  let response
  try {
    response = await fetchTimeout(pricingURL, { headers: { Accept: 'text/html' } }, 45_000)
  } catch (error) {
    const failure = sanitizedFailure(error)
    throw new Error(`upstream_request_failed code=${failure.code} status=${failure.status}`)
  }
  if (!response.ok) {
    await response.body?.cancel()
    throw sanitizedHTTPError(response)
  }
  const html = await response.text()
  const map = new Map()
  const rows = html.match(/<tr\b(?=[^>]*\bclass=["'][^"']*\btoken-model\b[^"']*["'])[^>]*>.*?<\/tr>/gis) || []
  for (const row of rows) {
    const name = row.match(/\bdata-model=["']([^"']+)["']/i)?.[1]
    if (!name) continue
    const cell = label => row.match(new RegExp(`<td\\b(?=[^>]*\\bdata-label=["']${label}["'])[^>]*>(.*?)<\\/td>`, 'is'))?.[1] || ''
    const strong = content => [...content.matchAll(/<strong\b[^>]*>(.*?)<\/strong>/gis)].map(match => parsePrice(match[1]))
    const input = strong(cell('输入'))[0]
    const output = strong(cell('输出'))[0]
    map.set(name.toLowerCase(), { input, output })
  }
  return map
}

const priceMap = await declaredPrices()
const modelResponse = await getJSON('/v1/models')
const allModels = (Array.isArray(modelResponse?.data) ? modelResponse.data : modelResponse)
  .map(item => typeof item === 'string' ? item : item?.id)
  .filter(Boolean)
const models = allModels.filter(model => requestedModels.has(model))
const availableModels = new Set(allModels)
const missingModels = requestedModelList.filter(model => !availableModels.has(model))
if (missingModels.length > 0) throw new Error(`Requested models not returned by /v1/models: ${missingModels.join(', ')}`)

const prefix = 'stable system cache knowledge alpha beta gamma delta epsilon zeta eta theta '.repeat(cachePrefixRepeats)
const results = []
let cursor = 0

const startUsage = await getJSON('/v1/usage')
const startCost = totalCost(startUsage)
const budgetGuard = new AtomicBudget(budget, startCost, reservationAmount)
const requestGuard = new AtomicRequestLimit(maximumRequests)

async function probeModel(model) {
  const prices = priceMap.get(model.toLowerCase())
  if (!prices || prices.input == null || prices.output == null) {
    return { model, error: 'missing_declared_input_output_for_residual' }
  }

  const session = `pricing-cache-${model}-${crypto.randomUUID()}`
  const plainBody = {
    model,
    messages: [
      { role: 'system', content: prefix },
      { role: 'user', content: 'Reply exactly A.' }
    ],
    max_tokens: 1,
    stream: false
  }
  const first = await send(model, plainBody, session)
  const second = first.delta?.requests === 1 && !budgetGuard.stopped && !requestGuard.stopped
    ? await send(model, plainBody, session)
    : { error: first.delta?.requests === 1 ? 'probe_stopped' : 'first_request_not_billed_once' }
  let rows = [first, second]

  // If normal sticky repetition produces no cache telemetry, try the standard
  // cache_control shape once. Unsupported models fail closed without retries.
  const noCache = rows.every(row => !row?.delta?.cache_write && !row?.delta?.cache_read)
  let explicit = []
  if (noCache && !budgetGuard.stopped && !requestGuard.stopped) {
    const explicitSession = `pricing-cache-explicit-${model}-${crypto.randomUUID()}`
    const explicitBody = {
      model,
      messages: [
        {
          role: 'system',
          content: [{ type: 'text', text: prefix, cache_control: { type: 'ephemeral' } }]
        },
        { role: 'user', content: 'Reply exactly A.' }
      ],
      max_tokens: 1,
      stream: false
    }
    const explicitFirst = await send(model, explicitBody, explicitSession)
    const explicitSecond = explicitFirst.delta?.requests === 1 && !budgetGuard.stopped && !requestGuard.stopped
      ? await send(model, explicitBody, explicitSession)
      : { error: explicitFirst.delta?.requests === 1 ? 'probe_stopped' : 'explicit_first_request_not_billed_once' }
    explicit = [explicitFirst, explicitSecond]
    rows = rows.concat(explicit)
  }

  return {
    model,
    input_per_million: prices.input,
    output_per_million: prices.output,
    ...solveCache(rows, prices.input, prices.output),
    plain: [first, second],
    explicit
  }
}

async function worker() {
  while (!budgetGuard.stopped && !requestGuard.stopped) {
    const index = cursor++
    if (index >= models.length) return
    const result = await probeModel(models[index])
    if (
      Array.isArray(result.plain) &&
      ![...result.plain, ...(result.explicit || [])].some(row => row?.dispatched)
    ) return
    results.push(result)
    console.log(`PROGRESS ${results.length}/${models.length} ${result.model} read=${result.cache_read_per_million ?? '-'} write=${result.cache_write_per_million ?? '-'}`)
  }
}

await Promise.all(Array.from({ length: concurrency }, () => worker()))
const endUsage = await getJSON('/v1/usage')
const endCost = totalCost(endUsage)
budgetGuard.observe(endCost)

console.log(`RESULT ${JSON.stringify({
  models: results.sort((a, b) => a.model.localeCompare(b.model)),
  budget,
  reservation_per_request: reservationAmount,
  budget_state: budgetGuard.snapshot(),
  budget_stopped: budgetGuard.stopped,
  request_limit_state: requestGuard.snapshot(),
  start_cost: startCost,
  end_cost: endCost,
  run_cost: Number((endCost - startCost).toFixed(12))
})}`)
