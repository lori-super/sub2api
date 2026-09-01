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

  it('supports optional provider and model overrides for all token dimensions', () => {
    for (const field of [
      'input_multiplier_override',
      'output_multiplier_override',
      'cache_write_multiplier_override',
      'cache_read_multiplier_override'
    ]) {
      expect(source).toContain(`v-model.number="providerForm[field.key]"`)
      expect(source).toContain(`${field}: null`)
      expect(source).toContain(`${field}: isToken ? nullableNumber(form.${field}) : null`)
    }
    expect(source).toContain("admin.displayPricing.dimensionOverrides.modelHint")
  })

  it('supports model final display-price overrides when a multiplier formula is insufficient', () => {
    for (const field of [
      'display_input_per_million_override',
      'display_output_per_million_override',
      'display_cache_write_per_million_override',
      'display_cache_read_per_million_override'
    ]) {
      expect(source).toContain(`${field}: null`)
      expect(source).toContain(`${field}: isToken ? nullableNumber(form.${field}) : null`)
    }
    expect(source).toContain('v-for="field in displayPriceOverrideFields"')
    expect(source).toContain("admin.displayPricing.priceOverrides.hint")
  })

  it('offers a display-only upstream token-price sync and refreshes the catalogue afterward', () => {
    expect(source).toContain('data-testid="upstream-token-sync"')
    expect(source).toContain('adminAPI.displayPricing.syncUpstreamTokenDisplayPrices()')
    expect(source).toContain('await loadData()')
    expect(source).toContain("admin.displayPricing.upstreamSync.hint")
    expect(source).toContain('notifyDisplayPricingUpdated()')
  })
})
