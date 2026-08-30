import assert from 'node:assert/strict'
import test from 'node:test'

import {
  AtomicBudget,
  AtomicRequestLimit,
  requiredProbeModels,
  sanitizedHTTPError,
  validatePricingURL,
  validateProbeTarget
} from './x5m5x-probe-safety.mjs'

const ENV_NAMES = [
  'X5M5X_ALLOW_INSECURE_LOCAL',
  'X5M5X_CONFIRM_LIVE_PROBE',
  'X5M5X_PROBE_MODELS'
]

function withCleanEnvironment(run) {
  const original = Object.fromEntries(ENV_NAMES.map(name => [name, process.env[name]]))
  for (const name of ENV_NAMES) delete process.env[name]
  try {
    return run()
  } finally {
    for (const name of ENV_NAMES) {
      if (original[name] === undefined) delete process.env[name]
      else process.env[name] = original[name]
    }
  }
}

test('real x5m5x targets require the exact paid-probe confirmation', { concurrency: false }, () => {
  withCleanEnvironment(() => {
    assert.throws(
      () => validateProbeTarget('https://us-api.x5m5x.com', 'test-key'),
      /confirmation_required/
    )
    process.env.X5M5X_CONFIRM_LIVE_PROBE = 'I_UNDERSTAND_THIS_COSTS_MONEY'
    assert.equal(validateProbeTarget('https://us-api.x5m5x.com', 'test-key'), 'https://us-api.x5m5x.com')
    assert.equal(validatePricingURL('https://api.x5m5x.com/pricing/'), 'https://api.x5m5x.com/pricing/')
  })
})

test('loopback mock works without live confirmation and rejects real-style keys', { concurrency: false }, () => {
  withCleanEnvironment(() => {
    process.env.X5M5X_ALLOW_INSECURE_LOCAL = 'true'
    assert.equal(validateProbeTarget('http://127.0.0.1:18085', 'fake-probe-key'), 'http://127.0.0.1:18085')
    assert.equal(validatePricingURL('http://localhost:18085/pricing/'), 'http://localhost:18085/pricing/')
    assert.throws(
      () => validateProbeTarget('http://127.0.0.1:18085', 'sk-real-style-key'),
      /real-style keys are refused/
    )
  })
})

test('an explicit non-empty model allowlist is mandatory and deduplicated', { concurrency: false }, () => {
  withCleanEnvironment(() => {
    assert.throws(() => requiredProbeModels(), /X5M5X_PROBE_MODELS is required/)
    process.env.X5M5X_PROBE_MODELS = 'mock-alpha, mock-beta, mock-alpha'
    assert.deepEqual(requiredProbeModels(), ['mock-alpha', 'mock-beta'])
  })
})

test('request limiter is an atomic hard cap across asynchronous workers', async () => {
  const limiter = new AtomicRequestLimit(4)
  const accepted = await Promise.all(
    Array.from({ length: 20 }, async () => limiter.acquire())
  )
  assert.equal(accepted.filter(Boolean).length, 4)
  assert.deepEqual(limiter.snapshot(), { limit: 4, dispatched: 4, stopped: true })
})

test('best-effort budget stops all later reservations when capacity is exhausted', () => {
  const budget = new AtomicBudget(0.01, 10, 0.006)
  assert.ok(budget.reserve('first'))
  assert.equal(budget.reserve('second'), null)
  assert.equal(budget.snapshot().stopped, true)
})

test('sanitized HTTP errors contain status and code but no upstream body', () => {
  const error = sanitizedHTTPError({ status: 429, body: 'secret upstream response' })
  assert.equal(error.code, 'upstream_http_error')
  assert.equal(error.status, 429)
  assert.match(error.message, /status=429/)
  assert.doesNotMatch(error.message, /secret upstream response/)
})
