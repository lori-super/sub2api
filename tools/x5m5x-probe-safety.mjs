const PRODUCTION_HOSTS = new Set(['api.x5m5x.com', 'us-api.x5m5x.com'])
const LOOPBACK_HOSTS = new Set(['127.0.0.1', '::1', 'localhost'])
const LOCAL_FAKE_KEY = /^(?:fake|mock|test)[-_][A-Za-z0-9._-]+$/i
const LIVE_PROBE_CONFIRMATION = 'I_UNDERSTAND_THIS_COSTS_MONEY'

function enabled(value) {
  return String(value || '').trim().toLowerCase() === 'true'
}

function cleanURL(raw, name) {
  let url
  try {
    url = new URL(raw)
  } catch {
    throw new Error(`${name} must be an absolute URL`)
  }
  if (url.username || url.password || url.search || url.hash) {
    throw new Error(`${name} must not contain credentials, query parameters, or a fragment`)
  }
  return url
}

function requireLiveProbeConfirmation() {
  if (process.env.X5M5X_CONFIRM_LIVE_PROBE !== LIVE_PROBE_CONFIRMATION) {
    throw new Error(
      `live_probe_confirmation_required code=confirmation_required; set X5M5X_CONFIRM_LIVE_PROBE=${LIVE_PROBE_CONFIRMATION}`
    )
  }
}

export function validateProbeTarget(rawBase, apiKey) {
  const url = cleanURL(rawBase, 'X5M5X_API_BASE')
  const host = url.hostname.toLowerCase()
  const localSimulation = enabled(process.env.X5M5X_ALLOW_INSECURE_LOCAL)

  if (PRODUCTION_HOSTS.has(host)) {
    if (url.protocol !== 'https:') {
      throw new Error('Production x5m5x probes require HTTPS')
    }
    requireLiveProbeConfirmation()
  } else if (LOOPBACK_HOSTS.has(host)) {
    if (!localSimulation || url.protocol !== 'http:') {
      throw new Error('Loopback HTTP requires X5M5X_ALLOW_INSECURE_LOCAL=true')
    }
    if (!LOCAL_FAKE_KEY.test(apiKey)) {
      throw new Error('Loopback simulation requires a fake-/mock-/test- prefixed key; real-style keys are refused')
    }
  } else {
    throw new Error(`X5M5X_API_BASE host is not allowed: ${host}`)
  }

  return url.toString().replace(/\/$/, '')
}

export function validatePricingURL(rawURL) {
  const url = cleanURL(rawURL, 'X5M5X_PRICING_URL')
  const host = url.hostname.toLowerCase()
  if (host === 'api.x5m5x.com' && url.protocol === 'https:') {
    requireLiveProbeConfirmation()
    return url.toString()
  }
  if (
    LOOPBACK_HOSTS.has(host) &&
    url.protocol === 'http:' &&
    enabled(process.env.X5M5X_ALLOW_INSECURE_LOCAL)
  ) {
    return url.toString()
  }
  throw new Error(`X5M5X_PRICING_URL is not an allowed x5m5x or loopback URL: ${url.origin}`)
}

export function positiveNumber(name, fallback) {
  const value = Number(process.env[name] || fallback)
  if (!Number.isFinite(value) || value <= 0) throw new Error(`${name} must be positive`)
  return value
}

export function positiveInteger(name, fallback) {
  const raw = process.env[name] === undefined ? fallback : process.env[name]
  const value = Number(raw)
  if (!Number.isInteger(value) || value <= 0) throw new Error(`${name} must be a positive integer`)
  return value
}

export function requiredProbeModels() {
  const models = (process.env.X5M5X_PROBE_MODELS || '')
    .split(',')
    .map(value => value.trim())
    .filter(Boolean)
  if (models.length === 0) {
    throw new Error('X5M5X_PROBE_MODELS is required and must contain at least one explicit model')
  }
  return [...new Set(models)]
}

export function boundedConcurrency(fallback = 1, maximum = 5) {
  const value = Number(process.env.X5M5X_PROBE_CONCURRENCY || fallback)
  if (!Number.isInteger(value) || value < 1 || value > maximum) {
    throw new Error(`X5M5X_PROBE_CONCURRENCY must be an integer from 1 to ${maximum}`)
  }
  return value
}

// This is a soft, best-effort cost guard. The upstream only reveals actual cost
// after a request, so one request can exceed the remaining budget. Reservations
// reduce that risk and stop new dispatch as soon as the estimate or ledger says
// the run is out of room, but they are not a hard monetary cap.
export class AtomicBudget {
  constructor(limit, startCost, defaultReservation) {
    this.limit = limit
    this.startCost = startCost
    this.confirmed = 0
    this.reserved = 0
    this.defaultReservation = defaultReservation
    this.nextID = 1
    this.active = new Map()
    this.stopped = false
  }

  observe(totalCost) {
    if (Number.isFinite(totalCost)) {
      this.confirmed = Math.max(this.confirmed, totalCost - this.startCost)
    }
    if (this.confirmed >= this.limit) this.stopped = true
  }

  reserve(label, amount = this.defaultReservation) {
    if (this.stopped) return null
    const normalized = Math.max(0, amount)
    if (!Number.isFinite(normalized) || normalized <= 0) {
      throw new Error('probe reservation must be positive')
    }
    if (this.confirmed + this.reserved + normalized > this.limit + 1e-12) {
      this.stopped = true
      return null
    }
    const token = { id: this.nextID++, label, amount: normalized }
    this.active.set(token.id, token)
    this.reserved += normalized
    return token
  }

  settle(token, totalCost) {
    if (token && this.active.delete(token.id)) {
      this.reserved = Math.max(0, this.reserved - token.amount)
    }
    this.observe(totalCost)
  }

  cancel(token) {
    if (token && this.active.delete(token.id)) {
      this.reserved = Math.max(0, this.reserved - token.amount)
    }
  }

  halt() {
    this.stopped = true
  }

  snapshot() {
    return {
      limit: this.limit,
      confirmed: Number(this.confirmed.toFixed(12)),
      reserved: Number(this.reserved.toFixed(12)),
      stopped: this.stopped
    }
  }
}

// JavaScript executes acquire() synchronously, so this check-and-increment is an
// atomic hard cap across all async workers. It counts attempted billable POSTs;
// a transport failure still consumes its slot because dispatch may have reached
// the upstream.
export class AtomicRequestLimit {
  constructor(limit) {
    if (!Number.isInteger(limit) || limit <= 0) {
      throw new Error('request limit must be a positive integer')
    }
    this.limit = limit
    this.dispatched = 0
    this.stopped = false
  }

  acquire() {
    if (this.stopped || this.dispatched >= this.limit) {
      this.stopped = true
      return false
    }
    this.dispatched++
    if (this.dispatched >= this.limit) this.stopped = true
    return true
  }

  halt() {
    this.stopped = true
  }

  snapshot() {
    return {
      limit: this.limit,
      dispatched: this.dispatched,
      stopped: this.stopped
    }
  }
}

export function sanitizedHTTPError(response) {
  const error = new Error(`upstream_request_failed code=upstream_http_error status=${response.status}`)
  error.code = 'upstream_http_error'
  error.status = response.status
  return error
}

export function sanitizedFailure(error, status = undefined) {
  if (typeof error?.code === 'string' && /^upstream_[a-z_]+$/.test(error.code)) {
    return { code: error.code, status: Number(error.status) || status || 0 }
  }
  if (error?.name === 'AbortError') return { code: 'upstream_timeout', status: status || 0 }
  return { code: 'upstream_transport_error', status: status || 0 }
}
