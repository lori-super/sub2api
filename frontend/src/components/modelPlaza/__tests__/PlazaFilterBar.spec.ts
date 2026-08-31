import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import PlazaFilterBar from '../PlazaFilterBar.vue'

vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key })
  }
})

function mountFilter() {
  return mount(PlazaFilterBar, {
    props: {
      billingOptions: [
        { value: 'all', label: '全部', count: 2 },
        { value: 'token', label: '按量', count: 2 }
      ],
      billingMode: 'all',
      providers: [
        { value: 'openai', label: 'OpenAI', count: 2, logoKey: 'openai', logoUrl: '' }
      ],
      platform: 'all',
      search: '',
      resultCount: 2,
      totalCount: 2,
      lastUpdatedLabel: '12:00:00',
      refreshing: false
    },
    global: {
      stubs: {
        Icon: true,
        ProviderLogo: true
      }
    }
  })
}

describe('PlazaFilterBar responsive behavior', () => {
  it('is normal-flow on mobile and only sticky on desktop', () => {
    const wrapper = mountFilter()
    const filter = wrapper.get('[data-testid="model-plaza-filter"]')

    expect(filter.classes()).not.toContain('sticky')
    expect(filter.classes()).toEqual(expect.arrayContaining(['w-full', 'max-w-full', 'lg:sticky', 'lg:top-4']))
  })

  it('keeps provider navigation and search in a collapsible mobile panel', async () => {
    const wrapper = mountFilter()
    const toggle = wrapper.get('[data-testid="mobile-filter-toggle"]')
    const panel = wrapper.get('[data-testid="model-plaza-filter-content"]')

    expect(toggle.attributes('aria-expanded')).toBe('false')
    expect(panel.classes()).toEqual(expect.arrayContaining(['hidden', 'lg:block']))
    expect(panel.text()).toContain('modelPlaza.filters.providerNavigation')
    expect(panel.text()).toContain('modelPlaza.filters.modelSearch')

    await toggle.trigger('click')

    expect(toggle.attributes('aria-expanded')).toBe('true')
    expect(panel.classes()).toContain('block')
    expect(panel.classes()).not.toContain('hidden')
  })
})
