import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'

const viewPath = resolve(dirname(fileURLToPath(import.meta.url)), '../DisplayPricingView.vue')
const source = readFileSync(viewPath, 'utf8')

describe('DisplayPricingView pricing rules', () => {
  it('keeps the global baseline fixed at one', () => {
    expect(source).toContain('const FIXED_GLOBAL_MULTIPLIER = 1')
    expect(source).toContain(':value="FIXED_GLOBAL_MULTIPLIER"')
    expect(source).not.toContain('adminAPI.displayPricing.updateSettings(')
  })

  it('edits only the base per-request tier and derives the remaining tiers', () => {
    expect(source).toContain('v-model="form.per_request_lte_256k"')
    expect(source).toContain(':model-value="derivedTier(1.5)"')
    expect(source).toContain(':model-value="derivedTier(2)"')
    expect(source).toContain('per_request_256k_512k_override: null')
    expect(source).toContain('per_request_gt_512k_override: null')
    expect(source).not.toContain('v-model="form.per_request_256k_512k_override"')
    expect(source).not.toContain('v-model="form.per_request_gt_512k_override"')
  })

  it('shows the backend error returned by provider and model saves', () => {
    expect(source).toContain("extractApiErrorMessage(error, t('admin.displayPricing.saveFailed'))")
  })
})
