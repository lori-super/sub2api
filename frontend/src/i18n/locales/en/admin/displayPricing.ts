export default {
  displayPricing: {
    title: 'Display Pricing',
    description: 'Manage the official base prices, display multipliers, per-request tiers, and image prices shown on the Model Prices page.',
    preview: 'Preview Model Prices',
    isolationNotice: 'Changes here affect display prices only. They do not change channel costs, group pricing, account routing, or actual user charges.',
    tabs: {
      label: 'Display pricing management',
      configuration: 'Display settings',
      official: 'Official prices'
    },
    loadFailed: 'Failed to load display pricing',
    saveFailed: 'Failed to save display pricing',
    saved: 'Display pricing saved',
    currency: 'Display currency',
    sortOrder: 'Order',
    global: {
      title: 'Global display multiplier',
      hint: 'The global baseline is fixed at 1×; set selling multipliers per provider',
      multiplier: 'Default multiplier',
      fixed: 'Fixed',
      priority: 'Normal models inherit their provider multiplier. Set a fixed multiplier only for exceptional models. Per-request pricing always uses the 1× / 1.5× / 2× curve.'
    },
    providers: {
      title: 'Provider display settings',
      hint: 'Create, edit, or delete provider names, logos, currencies, and token multiplier factors.',
      add: 'Add provider',
      createTitle: 'Add provider display settings',
      editTitle: 'Edit provider display settings',
      key: 'Provider key',
      name: 'Provider name',
      tokenNote: 'Usage-pricing note',
      tokenNotePlaceholder: 'Optional note shown only below this provider\'s usage-based pricing section',
      perRequestNote: 'Per-request note',
      perRequestNotePlaceholder: 'Optional note shown only below this provider\'s per-request pricing section',
      imageNote: 'Image-pricing note',
      imageNotePlaceholder: 'Optional note shown only below this provider\'s image-pricing section',
      multiplier: 'Provider multiplier factor',
      multiplierValue: 'Factor ×{value}',
      dimensionOverrides: 'Per-dimension rates',
      logoKey: 'Built-in logo',
      logoAuto: 'Match provider key automatically',
      logoUrl: 'Custom logo URL',
      logoPreview: 'Logo preview',
      logoHint: 'A custom URL takes priority. Only HTTPS or same-site relative paths are allowed; failures fall back to the built-in logo.',
      deleteTitle: 'Delete provider display settings',
      deleteMessage: 'Delete provider "{provider}"? All of its display-model settings will also be deleted, but real channels, routing, and billing will not be affected.',
      deleted: 'Provider and its display-model settings deleted',
      deleteFailed: 'Failed to delete provider display settings',
      empty: 'No provider settings'
    },
    models: {
      title: 'Model base prices and display rules',
      hint: 'Only enabled rules appear on the customer Model Prices page.',
      discover: 'Choose live model',
      add: 'Add display rule',
      search: 'Search model or provider',
      allProviders: 'All providers',
      allModes: 'All billing modes',
      model: 'Model',
      provider: 'Provider',
      note: 'Model note',
      notePlaceholder: 'Optional highlighted text shown below the model name, such as availability or launch information',
      mode: 'Billing mode',
      rule: 'Display rule',
      empty: 'No matching display pricing rules',
      inherited: 'inherited',
      tokenSummary: 'Official price × {multiplier}',
      tokenFixedSummary: 'Official price × fixed {multiplier}',
      tokenInheritedSummary: 'Official price × provider multiplier',
      tokenDimensionSummary: 'Per-dimension: {overrides}',
      tokenPriceOverrideSummary: 'Final price overrides: {overrides}',
      perRequestSummary: 'First tier {price}; next tiers are fixed at ×1.5 / ×2',
      imageSummary: '{count} image tiers'
    },
    editor: {
      createTitle: 'Add Display Pricing',
      editTitle: 'Edit Display Pricing',
      officialPrices: 'Official base prices (per 1M tokens)',
      tokenFormula: 'Our price uses an explicit final-price override first; otherwise it is the official base price × the effective multiplier for that dimension.',
      modelMultiplier: 'Fixed display multiplier',
      inheritMultiplier: 'Leave blank to follow the provider multiplier',
      perRequestPrices: 'Three-tier per-request prices',
      perRequestFormula: 'Enter only the ≤256K 1× price. The next tiers are always derived at 1.5× / 2× and cannot be overridden separately.',
      tier2Derived: '256K–512K ·1.5× (automatic)',
      tier3Derived: '> 512K ·2× (automatic)',
      autoDerived: 'Auto-derived',
      imagePrices: 'Image specification prices',
      imageHint: 'Enter the base price for each specification. The customer page shows both the base price and the site price calculated with the effective multiplier.',
      addTier: 'Add tier',
      specLabel: 'Specification',
      pricePerImage: 'Base price per image'
    },
    dimensionOverrides: {
      title: 'Advanced: per-dimension multiplier overrides',
      providerHint: 'Use only when input, output, or cache dimensions have different selling multipliers. Leave blank to inherit the provider multiplier above.',
      modelHint: 'Model dimension overrides have the highest priority and suit exceptional prices such as cache reads. Leave blank to inherit the model or provider rule.',
      inheritProviderUnified: 'Inherit provider multiplier',
      inheritModelRule: 'Inherit parent rule'
    },
    priceOverrides: {
      title: 'Advanced: final price overrides (per 1M tokens)',
      hint: 'Use only when official base price × multiplier cannot represent the upstream price. These values override every multiplier; leave blank for normal multiplier calculation.'
    },
    discovered: {
      title: 'Choose a live model',
      search: 'Search models discovered from current channels',
      configured: 'Configured',
      configure: 'Configure price'
    },
    delete: {
      title: 'Delete Display Pricing',
      message: 'Delete the display price for {model}? This will not affect real model access or billing.',
      failed: 'Failed to delete display pricing'
    }
  }
}
