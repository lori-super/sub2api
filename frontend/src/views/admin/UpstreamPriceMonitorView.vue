<template>
  <AppLayout>
    <div class="w-full min-w-0 space-y-6 pb-8">
      <header
        class="rounded-3xl bg-white p-5 shadow-sm ring-1 ring-gray-900/5 dark:bg-dark-800 dark:ring-dark-700 sm:p-6"
      >
        <div class="flex flex-wrap items-start justify-between gap-4">
          <div>
            <h1 class="flex items-center gap-2 text-xl font-black text-gray-900 dark:text-white">
              <span
                class="inline-flex h-8 w-8 items-center justify-center rounded-xl bg-emerald-50 text-emerald-600 dark:bg-emerald-500/10 dark:text-emerald-300"
              >
                <Icon name="beaker" size="sm" />
              </span>
              {{ t('admin.upstreamPriceMonitor.title') }}
            </h1>
            <p class="mt-1.5 text-sm text-gray-500 dark:text-dark-400">
              {{ t('admin.upstreamPriceMonitor.description') }}
            </p>
          </div>
          <button
            type="button"
            class="btn btn-secondary"
            :disabled="refreshing"
            data-testid="refresh-monitor"
            @click="refreshCurrentTab"
          >
            <Icon name="refresh" size="sm" :class="refreshing ? 'animate-spin' : ''" />
            {{ t('admin.upstreamPriceMonitor.refresh') }}
          </button>
        </div>

        <nav
          class="mt-5 flex gap-1 overflow-x-auto border-t border-gray-100 pt-4 dark:border-dark-700"
          role="tablist"
        >
          <button
            v-for="tab in tabs"
            :key="tab.key"
            type="button"
            role="tab"
            class="tab whitespace-nowrap"
            :class="activeTab === tab.key ? 'tab-active' : ''"
            :aria-selected="activeTab === tab.key"
            :data-testid="`tab-${tab.key}`"
            @click="activeTab = tab.key"
          >
            {{ tab.label }}
          </button>
        </nav>
      </header>

      <div v-if="initialLoading" class="flex min-h-[320px] items-center justify-center">
        <span class="h-8 w-8 animate-spin rounded-full border-2 border-primary-600/25 border-t-primary-600"></span>
      </div>

      <template v-else>
        <section v-if="activeTab === 'overview'" class="space-y-5" data-testid="overview-panel">
          <div class="grid gap-4 sm:grid-cols-2 xl:grid-cols-5">
            <article class="monitor-stat-card">
              <span class="monitor-stat-label">{{ t('admin.upstreamPriceMonitor.runtime.status') }}</span>
              <span class="mt-2 inline-flex rounded-full px-2.5 py-1 text-xs font-semibold" :class="runtimeStatusClass">
                {{ statusLabel(runtime?.status || 'disabled') }}
              </span>
              <span class="mt-2 text-xs text-gray-400">{{ runtimeStatusHint }}</span>
            </article>
            <article class="monitor-stat-card">
              <span class="monitor-stat-label">{{ t('admin.upstreamPriceMonitor.runtime.mode') }}</span>
              <strong class="monitor-stat-value text-base">{{ modeLabel(configDraft.mode) }}</strong>
              <span class="mt-2 text-xs text-gray-400">{{ modeShortHint(configDraft.mode) }}</span>
            </article>
            <article class="monitor-stat-card">
              <span class="monitor-stat-label">{{ t('admin.upstreamPriceMonitor.runtime.coverage') }}</span>
              <strong class="monitor-stat-value">{{ coverageText }}</strong>
              <div class="mt-2 h-1.5 overflow-hidden rounded-full bg-gray-100 dark:bg-dark-700">
                <div class="h-full rounded-full bg-emerald-500 transition-all" :style="{ width: `${coveragePercent}%` }"></div>
              </div>
            </article>
            <article class="monitor-stat-card">
              <span class="monitor-stat-label">{{ t('admin.upstreamPriceMonitor.runtime.nextRun') }}</span>
              <strong class="monitor-stat-value text-base">{{ formatTimestamp(runtime?.next_run_at, 'next') }}</strong>
              <span class="mt-2 text-xs text-gray-400">{{ intervalLabel(configDraft.interval_minutes) }}</span>
            </article>
            <article class="monitor-stat-card">
              <span class="monitor-stat-label">{{ t('admin.upstreamPriceMonitor.runtime.todayCost') }}</span>
              <strong class="monitor-stat-value font-mono" data-testid="today-probe-cost">{{ formatMoney(runtime?.today_probe_cost) }}</strong>
              <span class="mt-2 text-xs text-gray-400">
                {{ t('admin.upstreamPriceMonitor.runtime.dailyBudget', { value: formatMoney(configDraft.active_probe_daily_budget_usd) }) }}
              </span>
            </article>
          </div>

          <div
            class="flex flex-col gap-4 rounded-2xl border px-4 py-4 sm:flex-row sm:items-center sm:justify-between"
            :class="modeBannerClass"
          >
            <div class="flex min-w-0 items-start gap-3">
              <Icon :name="configDraft.mode === 'auto_apply' ? 'bolt' : configDraft.mode === 'review' ? 'clipboard' : 'eye'" size="sm" class="mt-0.5 shrink-0" />
              <div>
                <p class="font-semibold text-gray-900 dark:text-white">{{ modeLabel(configDraft.mode) }}</p>
                <p class="mt-1 text-xs leading-5 text-gray-600 dark:text-dark-300">
                  {{ modeLongHint(configDraft.mode) }}
                </p>
              </div>
            </div>
            <div class="flex shrink-0 flex-wrap gap-2">
              <button
                type="button"
                class="btn btn-primary"
                :disabled="creatingRun || runtime?.status === 'running'"
                data-testid="run-now"
                @click="handleRunNow"
              >
                <Icon name="play" size="sm" />
                {{ creatingRun ? t('admin.upstreamPriceMonitor.overview.running') : t('admin.upstreamPriceMonitor.overview.runNow') }}
              </button>
              <button type="button" class="btn btn-secondary" data-testid="open-config" @click="openConfigPanel">
                <Icon name="cog" size="sm" />
                {{ t('admin.upstreamPriceMonitor.overview.rules') }}
              </button>
              <button
                v-if="configDraft.mode === 'review' && latestApplicableRun"
                type="button"
                class="btn btn-secondary"
                data-testid="apply-latest"
                @click="requestRunAction('apply', latestApplicableRun)"
              >
                {{ t('admin.upstreamPriceMonitor.overview.apply') }}
              </button>
              <button
                v-if="configDraft.mode !== 'observe' && latestRollbackRun"
                type="button"
                class="btn btn-secondary text-amber-700 dark:text-amber-300"
                data-testid="rollback-latest"
                @click="requestRunAction('rollback', latestRollbackRun)"
              >
                {{ t('admin.upstreamPriceMonitor.overview.rollback') }}
              </button>
            </div>
          </div>

          <div
            v-if="runtime?.key_exclusive === false || hasReconciliationMismatch"
            class="flex items-start gap-2 rounded-2xl border border-amber-200 bg-amber-50 px-4 py-3 text-sm text-amber-800 dark:border-amber-500/25 dark:bg-amber-500/10 dark:text-amber-200"
            data-testid="key-exclusive-warning"
          >
            <Icon name="exclamationTriangle" size="sm" class="mt-0.5 shrink-0" />
            {{ t('admin.upstreamPriceMonitor.config.exclusiveWarning') }}
          </div>

          <div
            v-if="runtime?.last_error"
            class="rounded-2xl border border-amber-200 bg-amber-50 px-4 py-3 dark:border-amber-500/25 dark:bg-amber-500/10"
          >
            <p class="text-xs font-semibold uppercase tracking-wide text-amber-700 dark:text-amber-300">
              {{ t('admin.upstreamPriceMonitor.runtime.errorTitle') }}
            </p>
            <p class="mt-1 break-words text-sm text-amber-800 dark:text-amber-200">{{ localizedMonitorError(runtime.last_error) }}</p>
          </div>

          <article class="monitor-panel overflow-hidden">
            <div class="flex flex-col gap-4 border-b border-gray-100 px-5 py-4 dark:border-dark-700 lg:flex-row lg:items-end lg:justify-between">
              <div>
                <h2 class="font-bold text-gray-900 dark:text-white">{{ t('admin.upstreamPriceMonitor.overview.evidenceTitle') }}</h2>
                <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">{{ t('admin.upstreamPriceMonitor.overview.evidenceHint') }}</p>
              </div>
              <div class="grid gap-2 sm:grid-cols-[minmax(180px,260px)_180px]">
                <input
                  v-model.trim="evidenceSearch"
                  class="input"
                  :placeholder="t('admin.upstreamPriceMonitor.overview.search')"
                  data-testid="evidence-search"
                />
                <select v-model="evidenceStatus" class="input" data-testid="evidence-status-filter">
                  <option value="">{{ t('admin.upstreamPriceMonitor.overview.allStatuses') }}</option>
                  <option v-for="status in evidenceStatuses" :key="status" :value="status">{{ statusLabel(status) }}</option>
                </select>
              </div>
            </div>

            <div class="divide-y divide-gray-100 dark:divide-dark-700/70">
              <article
                v-for="item in filteredEvidence"
                :key="`${item.account_id}:${item.model}`"
                class="px-4 py-5 transition-colors hover:bg-gray-50/60 dark:hover:bg-dark-700/20 sm:px-5"
              >
                <div class="grid gap-5 lg:grid-cols-[minmax(180px,1.15fr)_repeat(3,minmax(145px,1fr))_minmax(120px,.8fr)]">
                  <div class="min-w-0">
                    <p class="break-all font-mono text-sm font-bold text-gray-900 dark:text-white">{{ item.model }}</p>
                    <div class="mt-2 flex flex-wrap gap-1.5">
                      <span class="monitor-chip">{{ item.billing_mode === 'per_request' ? t('admin.upstreamPriceMonitor.overview.billingRequest') : t('admin.upstreamPriceMonitor.overview.billingToken') }}</span>
                      <span class="rounded-full px-2.5 py-1 text-[11px] font-semibold" :class="evidenceStatusClass(item.status)">{{ statusLabel(item.status) }}</span>
                    </div>
                    <p class="mt-2 text-xs text-gray-400">
                      {{ item.observed_at ? formatDateTime(item.observed_at) : t('admin.upstreamPriceMonitor.overview.noEvidenceTime') }}
                      · {{ t('admin.upstreamPriceMonitor.overview.samples', { count: item.sample_count }) }}
                    </p>
                  </div>

                  <div class="price-summary-column">
                    <p class="price-summary-title">{{ t('admin.upstreamPriceMonitor.overview.currentPrices') }}</p>
                    <div v-for="field in visiblePriceFields(item)" :key="`current-${field.key}`" class="price-summary-row">
                      <span>{{ field.label }}</span><strong>{{ formatPrice(field.current) }}</strong>
                    </div>
                    <p v-if="hasDisplayPrices(item)" class="mt-2 border-t border-gray-100 pt-2 text-[11px] font-semibold text-gray-400 dark:border-dark-700">
                      {{ t('admin.upstreamPriceMonitor.overview.displayPrices') }}
                    </p>
                    <template v-if="hasDisplayPrices(item)">
                      <div v-for="field in visiblePriceFields(item)" :key="`display-${field.key}`" class="price-summary-row text-gray-500">
                        <span>{{ field.label }}</span><strong>{{ formatPrice(field.display) }}</strong>
                      </div>
                    </template>
                  </div>

                  <div class="price-summary-column bg-blue-50/50 dark:bg-blue-500/5">
                    <p class="price-summary-title text-blue-700 dark:text-blue-300">{{ t('admin.upstreamPriceMonitor.overview.measuredPrices') }}</p>
                    <div v-for="field in visiblePriceFields(item)" :key="`observed-${field.key}`" class="price-summary-row">
                      <span class="flex items-center gap-1.5">
                        <i class="h-1.5 w-1.5 rounded-full" :class="dimensionDotClass(field.status)" />{{ field.label }}
                      </span>
                      <strong>{{ formatPrice(field.observed) }}</strong>
                    </div>
                  </div>

                  <div class="price-summary-column bg-emerald-50/60 dark:bg-emerald-500/5">
                    <p class="price-summary-title text-emerald-700 dark:text-emerald-300">{{ t('admin.upstreamPriceMonitor.overview.targetPrices') }}</p>
                    <div v-for="field in visiblePriceFields(item)" :key="`target-${field.key}`" class="price-summary-row">
                      <span>{{ field.label }}</span>
                      <strong :class="priceChanged(field.current, field.suggested) ? 'text-emerald-700 dark:text-emerald-300' : ''">{{ formatPrice(field.suggested) }}</strong>
                    </div>
                    <p class="mt-2 text-[11px] text-gray-400">{{ t('admin.upstreamPriceMonitor.overview.targetFormula') }}</p>
                  </div>

                  <div class="min-w-0 lg:text-right">
                    <p class="price-summary-title">{{ t('admin.upstreamPriceMonitor.overview.applyState') }}</p>
                    <span class="mt-2 inline-flex rounded-full px-2.5 py-1 text-xs font-semibold" :class="applyStateClass(item)">
                      {{ applyStateLabel(item) }}
                    </span>
                    <p class="mt-2 text-xs text-gray-500 dark:text-dark-400">{{ reconciliationLabel(item) }}</p>
                    <div v-if="item.last_error || isEvidenceMismatch(item)" class="mt-3 rounded-xl bg-amber-50 px-3 py-2 text-left text-xs leading-5 text-amber-800 dark:bg-amber-500/10 dark:text-amber-200">
                      {{ localizedMonitorError(item.last_error || item.reconciliation_status) }}
                    </div>
                  </div>
                </div>
              </article>
            </div>
            <p v-if="filteredEvidence.length === 0" class="px-5 py-12 text-center text-sm text-gray-400">
              {{ t('admin.upstreamPriceMonitor.overview.empty') }}
            </p>
          </article>
        </section>

        <details ref="configPanelRef" v-if="activeTab === 'overview'" class="monitor-panel group overflow-hidden" data-testid="config-panel">
          <summary class="flex cursor-pointer list-none items-center justify-between gap-4 px-5 py-4 [&::-webkit-details-marker]:hidden">
            <div>
              <p class="font-bold text-gray-900 dark:text-white">{{ t('admin.upstreamPriceMonitor.config.collapsedTitle') }}</p>
              <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">{{ t('admin.upstreamPriceMonitor.config.collapsedHint') }}</p>
            </div>
            <Icon name="chevronDown" size="sm" class="shrink-0 text-gray-400 transition-transform group-open:rotate-180" />
          </summary>
          <article class="border-t border-gray-100 p-5 dark:border-dark-700 sm:p-6">
            <div class="flex flex-wrap items-start justify-between gap-3 border-b border-gray-100 pb-5 dark:border-dark-700">
              <div>
                <h2 class="font-bold text-gray-900 dark:text-white">{{ t('admin.upstreamPriceMonitor.config.title') }}</h2>
                <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">{{ t('admin.upstreamPriceMonitor.config.hint') }}</p>
              </div>
              <span v-if="configDirty" class="rounded-full bg-amber-100 px-2.5 py-1 text-xs font-semibold text-amber-700 dark:bg-amber-500/15 dark:text-amber-300">
                {{ t('admin.upstreamPriceMonitor.config.unsaved') }}
              </span>
            </div>

            <div class="mt-5 grid gap-3 lg:grid-cols-2">
              <div
                class="flex items-start gap-3 rounded-2xl border border-blue-200 bg-blue-50 px-4 py-3 text-sm text-blue-800 dark:border-blue-500/25 dark:bg-blue-500/10 dark:text-blue-200"
                data-testid="active-only-notice"
              >
                <Icon name="bolt" size="sm" class="mt-0.5 shrink-0" />
                <div class="min-w-0">
                  <p class="font-semibold">{{ t('admin.upstreamPriceMonitor.config.activeOnly') }}</p>
                  <p class="mt-1 text-xs leading-5">{{ t('admin.upstreamPriceMonitor.config.activeOnlyHint') }}</p>
                </div>
              </div>
              <div
                class="flex items-start gap-3 rounded-2xl border border-emerald-200 bg-emerald-50 px-4 py-3 text-sm text-emerald-800 dark:border-emerald-500/25 dark:bg-emerald-500/10 dark:text-emerald-200"
                data-testid="change-only-notice"
              >
                <Icon name="bell" size="sm" class="mt-0.5 shrink-0" />
                <div class="min-w-0">
                  <p class="font-semibold">{{ t('admin.upstreamPriceMonitor.config.changeOnlyNotify') }}</p>
                  <p class="mt-1 text-xs leading-5">{{ t('admin.upstreamPriceMonitor.config.changeOnlyNotifyHint') }}</p>
                </div>
              </div>
              <div
                class="flex items-start gap-3 rounded-2xl border border-amber-200 bg-amber-50 px-4 py-3 text-sm text-amber-800 dark:border-amber-500/25 dark:bg-amber-500/10 dark:text-amber-200 lg:col-span-2"
                data-testid="exclusive-key-warning"
              >
                <Icon name="exclamationTriangle" size="sm" class="mt-0.5 shrink-0" />
                <div>
                  <p class="font-semibold">{{ t('admin.upstreamPriceMonitor.config.exclusiveTitle') }}</p>
                  <p class="mt-1 text-xs leading-5">{{ t('admin.upstreamPriceMonitor.config.exclusiveWarning') }}</p>
                </div>
              </div>
            </div>

            <form class="mt-6 space-y-7" @submit.prevent="saveConfig">
              <div class="grid gap-5 lg:grid-cols-2">
                <div class="config-toggle-row">
                  <div>
                    <p class="config-label">{{ t('admin.upstreamPriceMonitor.config.enabled') }}</p>
                    <p class="config-hint">{{ t('admin.upstreamPriceMonitor.config.enabledHint') }}</p>
                  </div>
                  <Toggle v-model="configDraft.enabled" data-testid="config-enabled" />
                </div>
                <div class="config-toggle-row" data-testid="active-probe-fixed">
                  <div>
                    <p class="config-label">{{ t('admin.upstreamPriceMonitor.config.probeStrategy') }}</p>
                    <p class="config-hint">{{ t('admin.upstreamPriceMonitor.config.probeStrategyHint') }}</p>
                  </div>
                  <span class="rounded-full bg-blue-100 px-2.5 py-1 text-xs font-semibold text-blue-700 dark:bg-blue-500/15 dark:text-blue-300">
                    {{ t('admin.upstreamPriceMonitor.config.fixed') }}
                  </span>
                </div>
              </div>

              <div class="grid gap-5 sm:grid-cols-2 xl:grid-cols-4">
                <label class="form-field">
                  <span class="field-label">{{ t('admin.upstreamPriceMonitor.config.mode') }}</span>
                  <select v-model="configDraft.mode" class="input mt-1.5" data-testid="config-mode">
                    <option value="observe">{{ t('admin.upstreamPriceMonitor.mode.observe') }}</option>
                    <option value="review">{{ t('admin.upstreamPriceMonitor.mode.review') }}</option>
                    <option value="auto_apply">{{ t('admin.upstreamPriceMonitor.mode.auto_apply') }}</option>
                  </select>
                  <span class="config-hint mt-1">{{ t('admin.upstreamPriceMonitor.config.modeHint') }}</span>
                </label>
                <label class="form-field">
                  <span class="field-label">{{ t('admin.upstreamPriceMonitor.config.interval') }}</span>
                  <select v-model.number="configDraft.interval_minutes" class="input mt-1.5" data-testid="config-interval">
                    <option v-for="option in intervalOptions" :key="option.value" :value="option.value">{{ option.label }}</option>
                  </select>
                  <span class="config-hint mt-1">{{ t('admin.upstreamPriceMonitor.config.intervalHint') }}</span>
                </label>
                <label class="form-field">
                  <span class="field-label">{{ t('admin.upstreamPriceMonitor.config.markup') }}</span>
                  <input v-model.number="configDraft.markup" type="number" min="1.2" max="1.2" step="0.01" readonly class="input mt-1.5 bg-gray-50 font-mono dark:bg-dark-900/30" data-testid="config-markup" />
                  <span class="config-hint mt-1">{{ t('admin.upstreamPriceMonitor.config.markupHint') }}</span>
                </label>
                <label class="form-field">
                  <span class="field-label">{{ t('admin.upstreamPriceMonitor.config.decimals') }}</span>
                  <input v-model.number="configDraft.display_multiplier_decimals" type="number" min="0" max="6" step="1" class="input mt-1.5 font-mono" data-testid="config-decimals" />
                </label>
                <label class="form-field">
                  <span class="field-label">{{ t('admin.upstreamPriceMonitor.config.maxModelsPerRun') }}</span>
                  <input v-model.number="configDraft.active_probe_max_models_per_run" type="number" min="1" max="19" step="1" class="input mt-1.5 font-mono" data-testid="config-max-models" />
                  <span class="config-hint mt-1">{{ t('admin.upstreamPriceMonitor.config.maxModelsPerRunHint') }}</span>
                </label>
                <label class="form-field">
                  <span class="field-label">{{ t('admin.upstreamPriceMonitor.config.maxRequestsPerModel') }}</span>
                  <input v-model.number="configDraft.active_probe_max_requests_per_model" type="number" min="1" max="7" step="1" class="input mt-1.5 font-mono" data-testid="config-max-requests" />
                  <span class="config-hint mt-1">{{ t('admin.upstreamPriceMonitor.config.maxRequestsPerModelHint') }}</span>
                </label>
                <label class="form-field">
                  <span class="field-label">{{ t('admin.upstreamPriceMonitor.config.runBudget') }}</span>
                  <input v-model.number="configDraft.active_probe_run_budget_usd" type="number" min="0.01" max="0.15" step="0.01" class="input mt-1.5 font-mono" data-testid="config-run-budget" />
                  <span class="config-hint mt-1">{{ t('admin.upstreamPriceMonitor.config.runBudgetHint') }}</span>
                </label>
                <label class="form-field">
                  <span class="field-label">{{ t('admin.upstreamPriceMonitor.config.dailyBudget') }}</span>
                  <input v-model.number="configDraft.active_probe_daily_budget_usd" type="number" min="0.01" max="0.40" step="0.01" class="input mt-1.5 font-mono" data-testid="config-daily-budget" />
                  <span class="config-hint mt-1">{{ t('admin.upstreamPriceMonitor.config.dailyBudgetHint') }}</span>
                </label>
              </div>

              <div class="grid gap-5 lg:grid-cols-2">
                <label class="form-field">
                  <span class="field-label">{{ t('admin.upstreamPriceMonitor.config.accounts') }}</span>
                  <select v-model="configDraft.account_ids" multiple class="input mt-1.5 min-h-[164px] py-2" data-testid="config-accounts">
                    <option v-for="account in accountOptions" :key="account.id" :value="account.id">
                      #{{ account.id }} · {{ account.name }} · {{ account.platform }}
                    </option>
                  </select>
                  <span class="config-hint mt-1">{{ accountOptions.length ? t('admin.upstreamPriceMonitor.config.accountsHint') : t('admin.upstreamPriceMonitor.config.noAccounts') }}</span>
                </label>
                <label class="form-field">
                  <span class="field-label">{{ t('admin.upstreamPriceMonitor.config.channels') }}</span>
                  <select v-model="configDraft.channel_ids" multiple class="input mt-1.5 min-h-[164px] py-2" data-testid="config-channels">
                    <option v-for="channel in channelOptions" :key="channel.id" :value="channel.id">
                      #{{ channel.id }} · {{ channel.name }}
                    </option>
                  </select>
                  <span class="config-hint mt-1">{{ channelOptions.length ? t('admin.upstreamPriceMonitor.config.channelsHint') : t('admin.upstreamPriceMonitor.config.noChannels') }}</span>
                </label>
              </div>

              <div class="grid gap-5 lg:grid-cols-2">
                <label class="form-field block">
                  <span class="field-label">{{ t('admin.upstreamPriceMonitor.config.models') }}</span>
                  <textarea
                    v-model="modelsText"
                    rows="9"
                    readonly
                    class="input mt-1.5 min-h-[150px] resize-y bg-gray-50 font-mono text-sm dark:bg-dark-900/30"
                    data-testid="config-models"
                  ></textarea>
                  <span class="config-hint mt-1">{{ t('admin.upstreamPriceMonitor.config.modelsHint') }}</span>
                </label>
                <label class="form-field block">
                  <span class="field-label">{{ t('admin.upstreamPriceMonitor.config.perRequestModels') }}</span>
                  <textarea
                    v-model="perRequestModelsText"
                    rows="9"
                    class="input mt-1.5 min-h-[150px] resize-y font-mono text-sm"
                    data-testid="config-per-request-models"
                  ></textarea>
                  <span class="config-hint mt-1">{{ t('admin.upstreamPriceMonitor.config.perRequestModelsHint') }}</span>
                </label>
              </div>

              <div class="overflow-hidden rounded-2xl border border-gray-200 dark:border-dark-700" data-testid="model-catalog">
                <div class="flex flex-col gap-3 border-b border-gray-100 p-4 dark:border-dark-700 sm:flex-row sm:items-end sm:justify-between">
                  <div>
                    <p class="config-label">{{ t('admin.upstreamPriceMonitor.config.catalogTitle') }}</p>
                    <p class="config-hint">{{ t('admin.upstreamPriceMonitor.config.catalogHint') }}</p>
                    <button type="button" class="btn btn-secondary mt-2 px-3 py-1.5 text-xs" :disabled="discoveringModels || configDirty" data-testid="discover-models" @click="discoverModels">
                      {{ discoveringModels ? t('admin.upstreamPriceMonitor.config.discoveringModels') : t('admin.upstreamPriceMonitor.config.discoverModels') }}
                    </button>
                  </div>
                  <div class="grid gap-2 sm:grid-cols-2">
                    <input v-model.trim="modelCatalogSearch" class="input" :placeholder="t('admin.upstreamPriceMonitor.config.catalogSearch')" />
                    <select v-model="modelCatalogStatus" class="input">
                      <option value="">{{ t('admin.upstreamPriceMonitor.config.catalogAll') }}</option>
                      <option v-for="status in ['managed','discovered','suspected_retired','ignored','retired']" :key="status" :value="status">
                        {{ t(`admin.upstreamPriceMonitor.config.modelStatus.${status}`) }}
                      </option>
                    </select>
                    <label class="flex items-center gap-2 text-xs text-gray-500 sm:col-span-2">
                      <input v-model="modelCatalogDomesticOnly" type="checkbox" />
                      {{ t('admin.upstreamPriceMonitor.config.domesticOnly') }}
                    </label>
                  </div>
                </div>
                <div class="max-h-[420px] overflow-auto">
                  <table class="w-full min-w-[760px] text-left text-sm">
                    <thead class="bg-gray-50 text-xs text-gray-500 dark:bg-dark-900/40 dark:text-dark-400">
                      <tr>
                        <th class="px-4 py-3">{{ t('admin.upstreamPriceMonitor.overview.model') }}</th>
                        <th class="px-4 py-3">{{ t('admin.upstreamPriceMonitor.config.catalogStatus') }}</th>
                        <th class="px-4 py-3">{{ t('admin.upstreamPriceMonitor.config.catalogCoverage') }}</th>
                        <th class="px-4 py-3">{{ t('admin.upstreamPriceMonitor.history.actions') }}</th>
                      </tr>
                    </thead>
                    <tbody class="divide-y divide-gray-100 dark:divide-dark-700">
                      <tr v-for="item in filteredModelCatalog" :key="item.model">
                        <td class="px-4 py-3">
                          <span class="font-mono font-semibold text-gray-900 dark:text-white">{{ item.model }}</span>
                          <span v-if="item.domestic_candidate" class="monitor-chip ml-2">{{ t('admin.upstreamPriceMonitor.config.domesticCandidate') }}</span>
                        </td>
                        <td class="px-4 py-3">
                          <span class="monitor-chip">{{ t(`admin.upstreamPriceMonitor.config.modelStatus.${item.status}`) }}</span>
                          <span v-if="item.missing_runs" class="ml-2 text-xs text-amber-600">{{ t('admin.upstreamPriceMonitor.config.missingRuns', { count: item.missing_runs }) }}</span>
                        </td>
                        <td class="px-4 py-3 font-mono text-xs">{{ item.seen_account_count }} / {{ item.expected_account_count }}</td>
                        <td class="px-4 py-3">
                          <div class="flex flex-wrap gap-2">
                            <button v-if="item.status !== 'managed'" type="button" class="btn btn-secondary px-2.5 py-1 text-xs" :data-testid="`manage-model-${item.model}`" :disabled="Boolean(updatingModel) || configDirty || !item.discovery_complete || item.expected_account_count === 0 || item.seen_account_count !== item.expected_account_count" @click="updateModelStatus(item.model, 'managed')">
                              {{ t('admin.upstreamPriceMonitor.config.manageModel') }}
                            </button>
                            <button v-if="item.status === 'discovered'" type="button" class="btn btn-secondary px-2.5 py-1 text-xs" :disabled="Boolean(updatingModel) || configDirty" @click="updateModelStatus(item.model, 'ignored')">
                              {{ t('admin.upstreamPriceMonitor.config.ignoreModel') }}
                            </button>
                            <button v-if="item.status === 'managed' || item.status === 'suspected_retired'" type="button" class="btn btn-secondary px-2.5 py-1 text-xs text-amber-700" :disabled="Boolean(updatingModel) || configDirty" @click="updateModelStatus(item.model, 'retired')">
                              {{ t('admin.upstreamPriceMonitor.config.retireModel') }}
                            </button>
                          </div>
                        </td>
                      </tr>
                      <tr v-if="filteredModelCatalog.length === 0"><td colspan="4" class="px-4 py-8 text-center text-sm text-gray-400">{{ t('admin.upstreamPriceMonitor.config.catalogEmpty') }}</td></tr>
                    </tbody>
                  </table>
                </div>
              </div>

              <p v-if="configError" class="rounded-xl bg-red-50 px-3 py-2 text-sm text-red-700 dark:bg-red-500/10 dark:text-red-300" data-testid="config-error">
                {{ configError }}
              </p>

              <div class="flex justify-end border-t border-gray-100 pt-5 dark:border-dark-700">
                <button type="submit" class="btn btn-primary" :disabled="savingConfig || !configDirty" data-testid="save-config">
                  {{ savingConfig ? t('admin.upstreamPriceMonitor.config.saving') : t('admin.upstreamPriceMonitor.config.save') }}
                </button>
              </div>
            </form>
          </article>
        </details>

        <section v-else class="space-y-5" data-testid="history-panel">
          <article class="monitor-panel overflow-hidden">
            <div class="flex flex-col gap-4 border-b border-gray-100 px-5 py-4 dark:border-dark-700 sm:flex-row sm:items-end sm:justify-between">
              <div>
                <h2 class="font-bold text-gray-900 dark:text-white">{{ t('admin.upstreamPriceMonitor.history.title') }}</h2>
                <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">{{ t('admin.upstreamPriceMonitor.history.hint') }}</p>
              </div>
              <select v-model="runStatus" class="input w-full sm:w-48" data-testid="history-status-filter" @change="handleHistoryFilter">
                <option value="">{{ t('admin.upstreamPriceMonitor.history.allStatuses') }}</option>
                <option v-for="status in runStatuses" :key="status" :value="status">{{ statusLabel(status) }}</option>
              </select>
            </div>
            <div class="divide-y divide-gray-100 dark:divide-dark-700/70">
              <article v-for="run in runs" :key="run.id" class="px-5 py-4 hover:bg-gray-50/60 dark:hover:bg-dark-700/20">
                <div class="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
                  <div class="flex min-w-0 items-start gap-3">
                    <span class="mt-0.5 inline-flex h-9 w-9 shrink-0 items-center justify-center rounded-xl" :class="runStatusClass(run.status)">
                      <Icon :name="run.status === 'running' ? 'refresh' : run.status === 'completed' ? 'check' : 'exclamationTriangle'" size="sm" :class="run.status === 'running' ? 'animate-spin' : ''" />
                    </span>
                    <div class="min-w-0">
                      <div class="flex flex-wrap items-center gap-2">
                        <p class="font-semibold text-gray-900 dark:text-white">{{ runSummaryTitle(run) }}</p>
                        <span class="monitor-chip">{{ triggerLabel(run.trigger) }}</span>
                        <span class="monitor-chip">{{ modeLabel(run.dry_run ? 'dry_run' : run.mode) }}</span>
                      </div>
                      <p class="mt-1 text-xs text-gray-500 dark:text-dark-400">
                        {{ formatDateTime(run.started_at) }} ·
                        {{ t('admin.upstreamPriceMonitor.history.matched', { count: run.matched_models }) }} ·
                        {{ t('admin.upstreamPriceMonitor.history.mismatched', { count: run.mismatched_models }) }}
                      </p>
                      <p v-if="run.error" class="mt-2 rounded-lg bg-amber-50 px-2.5 py-1.5 text-xs text-amber-800 dark:bg-amber-500/10 dark:text-amber-200">{{ localizedMonitorError(run.error) }}</p>
                    </div>
                  </div>
                  <div class="flex shrink-0 items-center justify-between gap-5 sm:justify-end">
                    <div class="text-right">
                      <p class="text-[11px] text-gray-400">{{ t('admin.upstreamPriceMonitor.history.cost') }}</p>
                      <p class="mt-0.5 font-mono text-sm font-semibold text-gray-800 dark:text-gray-100">{{ formatMoney(run.probe_cost) }}</p>
                    </div>
                    <button v-if="configDraft.mode === 'review' && canApply(run)" type="button" class="btn btn-primary px-3 py-1.5 text-xs" @click="requestRunAction('apply', run)">
                      {{ t('admin.upstreamPriceMonitor.overview.apply') }}
                    </button>
                    <button v-if="canRollback(run)" type="button" class="btn btn-secondary px-3 py-1.5 text-xs text-amber-700 dark:text-amber-300" @click="requestRunAction('rollback', run)">
                      {{ t('admin.upstreamPriceMonitor.overview.rollback') }}
                    </button>
                  </div>
                </div>
              </article>
            </div>
            <p v-if="runs.length === 0" class="px-5 py-12 text-center text-sm text-gray-400">{{ t('admin.upstreamPriceMonitor.history.empty') }}</p>
            <Pagination
              v-if="runsTotal > 0"
              :page="runsPage"
              :page-size="runsPageSize"
              :total="runsTotal"
              @update:page="handleRunsPage"
              @update:page-size="handleRunsPageSize"
            />
          </article>
        </section>
      </template>
    </div>

    <ConfirmDialog
      :show="Boolean(pendingRunAction)"
      :title="pendingRunAction?.type === 'rollback' ? t('admin.upstreamPriceMonitor.confirm.rollbackTitle') : t('admin.upstreamPriceMonitor.confirm.applyTitle')"
      :message="pendingRunAction?.type === 'rollback' ? t('admin.upstreamPriceMonitor.confirm.rollbackMessage') : t('admin.upstreamPriceMonitor.confirm.applyMessage')"
      :confirm-text="pendingRunAction?.type === 'rollback' ? t('admin.upstreamPriceMonitor.overview.rollback') : t('admin.upstreamPriceMonitor.overview.apply')"
      :danger="pendingRunAction?.type === 'rollback'"
      @confirm="confirmRunAction"
      @cancel="pendingRunAction = null"
    />
    <TotpStepUpDialog :controller="monitorStepUp" />
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import TotpStepUpDialog from '@/components/auth/TotpStepUpDialog.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import Pagination from '@/components/common/Pagination.vue'
import Toggle from '@/components/common/Toggle.vue'
import Icon from '@/components/icons/Icon.vue'
import accountsAPI from '@/api/admin/accounts'
import channelsAPI from '@/api/admin/channels'
import upstreamPriceMonitorAPI, {
  type UpstreamPriceEvidence,
  type UpstreamPriceDimension,
  type UpstreamPriceDimensionStatus,
  type UpstreamPriceEvidenceStatus,
  type UpstreamPriceMonitorConfig,
  type UpstreamPriceMonitorRun,
  type UpstreamPriceModelCatalogEntry,
  type UpstreamPriceModelStatus,
  type UpstreamPriceRunStatus,
} from '@/api/admin/upstreamPriceMonitor'
import type { Account } from '@/types'
import type { Channel } from '@/api/admin/channels'
import { useAppStore } from '@/stores/app'
import { isStepUpCancelled, useStepUp } from '@/composables/useStepUp'
import { extractApiErrorMessage } from '@/utils/apiError'
import { formatDateTime } from '@/utils/format'

type MonitorTab = 'overview' | 'history'
type PendingRunAction = { type: 'apply' | 'rollback'; run: UpstreamPriceMonitorRun }

const defaultConfig = (): UpstreamPriceMonitorConfig => ({
  enabled: false,
  mode: 'observe',
  interval_minutes: 360,
  markup: 1.2,
  display_multiplier_decimals: 3,
  account_ids: [],
  channel_ids: [],
  domestic_models: [],
  per_request_models: [],
  passive_sample_max_age_minutes: 1440,
  active_probe_enabled: true,
  active_only: true,
  active_probe_max_models_per_run: 19,
  active_probe_max_requests_per_model: 7,
  active_probe_run_budget_usd: 0.15,
  active_probe_daily_budget_usd: 0.40,
})

const { t } = useI18n()
const appStore = useAppStore()
const monitorStepUp = useStepUp()
const activeTab = ref<MonitorTab>('overview')
const initialLoading = ref(true)
const refreshing = ref(false)
const creatingRun = ref(false)
const savingConfig = ref(false)
const configError = ref('')
const runtime = ref<Awaited<ReturnType<typeof upstreamPriceMonitorAPI.getRuntime>> | null>(null)
const evidence = ref<UpstreamPriceEvidence[]>([])
const runs = ref<UpstreamPriceMonitorRun[]>([])
const runsTotal = ref(0)
const runsPage = ref(1)
const runsPageSize = ref(20)
const runStatus = ref<UpstreamPriceRunStatus | ''>('')
const evidenceStatus = ref<UpstreamPriceEvidenceStatus | ''>('')
const evidenceSearch = ref('')
const accountOptions = ref<Account[]>([])
const channelOptions = ref<Channel[]>([])
const configDraft = reactive<UpstreamPriceMonitorConfig>(defaultConfig())
const configBaseline = ref('')
const modelsText = ref('')
const perRequestModelsText = ref('')
const modelCatalog = ref<UpstreamPriceModelCatalogEntry[]>([])
const modelCatalogStatus = ref<UpstreamPriceModelStatus | ''>('')
const modelCatalogSearch = ref('')
const modelCatalogDomesticOnly = ref(true)
const updatingModel = ref('')
const discoveringModels = ref(false)
const pendingRunAction = ref<PendingRunAction | null>(null)
const configPanelRef = ref<HTMLDetailsElement | null>(null)

let loadController: AbortController | null = null
let pollTimer: ReturnType<typeof setInterval> | null = null

const tabs = computed(() => [
  { key: 'overview' as const, label: t('admin.upstreamPriceMonitor.tabs.overview') },
  { key: 'history' as const, label: t('admin.upstreamPriceMonitor.tabs.history') },
])
const intervalOptions = computed(() => [60, 180, 360, 720, 1440].map(value => ({
  value,
  label: intervalLabel(value),
})))
const evidenceStatuses: UpstreamPriceEvidenceStatus[] = ['trusted', 'pending', 'mismatch', 'stale', 'unobservable']
const runStatuses: UpstreamPriceRunStatus[] = ['running', 'completed', 'partial', 'failed']
const knownStatuses = new Set(['idle', 'running', 'degraded', 'failed', 'disabled', 'completed', 'partial', ...evidenceStatuses])
const knownModes = new Set(['observe', 'review', 'auto_apply', 'dry_run'])
const knownTriggers = new Set(['manual', 'scheduled', 'active_probe'])
const knownReconciliationStatuses = new Set([
  'baseline', 'matched', 'no_activity', 'mismatch', 'remote_reset', 'mixed_context',
  'closed', 'open', 'external_traffic',
])

function normalizedModels(): string[] {
  return Array.from(new Set(modelsText.value.split(/\r?\n|,/).map(value => value.trim()).filter(Boolean)))
}

function normalizedPerRequestModels(): string[] {
  return Array.from(new Set(perRequestModelsText.value.split(/\r?\n|,/).map(value => value.trim()).filter(Boolean)))
}

function configPayload(): UpstreamPriceMonitorConfig {
  return {
    ...configDraft,
    active_only: true,
    active_probe_enabled: true,
    markup: 1.2,
    account_ids: [...configDraft.account_ids].map(Number).filter(Number.isFinite).sort((a, b) => a - b),
    channel_ids: [...configDraft.channel_ids].map(Number).filter(Number.isFinite).sort((a, b) => a - b),
    domestic_models: normalizedModels().sort((a, b) => a.localeCompare(b)),
    per_request_models: normalizedPerRequestModels().sort((a, b) => a.localeCompare(b)),
  }
}

function serializeConfig(value: UpstreamPriceMonitorConfig): string {
  return JSON.stringify({
    ...value,
    account_ids: [...value.account_ids].sort((a, b) => a - b),
    channel_ids: [...value.channel_ids].sort((a, b) => a - b),
    domestic_models: [...value.domestic_models].sort((a, b) => a.localeCompare(b)),
    per_request_models: [...value.per_request_models].sort((a, b) => a.localeCompare(b)),
  })
}

const configDirty = computed(() => serializeConfig(configPayload()) !== configBaseline.value)
const coveragePercent = computed(() => {
  const total = runtime.value?.coverage?.total || 0
  if (!total) return 0
  return Math.min(100, Math.round(((runtime.value?.coverage?.trusted || 0) / total) * 100))
})
const coverageText = computed(() => `${runtime.value?.coverage?.trusted || 0} / ${runtime.value?.coverage?.total || 0}`)
const runtimeStatusClass = computed(() => runStatusClass(runtime.value?.status || 'disabled'))
const runtimeStatusHint = computed(() => {
  if (!configDraft.enabled) return t('admin.upstreamPriceMonitor.runtime.statusHintDisabled')
  if (runtime.value?.status === 'running') return t('admin.upstreamPriceMonitor.runtime.statusHintRunning')
  if (runtime.value?.status === 'failed' || runtime.value?.status === 'degraded') return t('admin.upstreamPriceMonitor.runtime.statusHintAttention')
  return t('admin.upstreamPriceMonitor.runtime.statusHintHealthy')
})
const modeBannerClass = computed(() => {
  if (configDraft.mode === 'auto_apply') return 'border-emerald-200 bg-emerald-50/70 dark:border-emerald-500/25 dark:bg-emerald-500/10'
  if (configDraft.mode === 'review') return 'border-violet-200 bg-violet-50/70 dark:border-violet-500/25 dark:bg-violet-500/10'
  return 'border-blue-200 bg-blue-50/70 dark:border-blue-500/25 dark:bg-blue-500/10'
})
const filteredEvidence = computed(() => {
  const needle = evidenceSearch.value.toLowerCase()
  return evidence.value.filter(item => {
    if (evidenceStatus.value && item.status !== evidenceStatus.value) return false
    return !needle || item.model.toLowerCase().includes(needle)
  })
})
const filteredModelCatalog = computed(() => {
  const needle = modelCatalogSearch.value.toLowerCase()
  return modelCatalog.value.filter(item => {
    if (modelCatalogStatus.value && item.status !== modelCatalogStatus.value) return false
    if (modelCatalogDomesticOnly.value && item.status === 'discovered' && !item.domestic_candidate) return false
    return !needle || item.model.toLowerCase().includes(needle)
  })
})
const hasReconciliationMismatch = computed(() => evidence.value.some(isEvidenceMismatch))
const latestApplicableRun = computed(() => runs.value.find(canApply) || null)
const latestRollbackRun = computed(() => runs.value.find(canRollback) || null)

function assignConfig(config: UpstreamPriceMonitorConfig): void {
  const normalized = { ...defaultConfig(), ...config }
  Object.assign(configDraft, normalized, {
    active_only: true,
    active_probe_enabled: true,
    markup: 1.2,
    account_ids: [...(normalized.account_ids || [])],
    channel_ids: [...(normalized.channel_ids || [])],
		domestic_models: [...(normalized.domestic_models || [])],
		per_request_models: [...(normalized.per_request_models || [])],
  })
  modelsText.value = normalized.domestic_models.join('\n')
  perRequestModelsText.value = normalized.per_request_models.join('\n')
  configBaseline.value = serializeConfig(configPayload())
}

async function loadOptions(): Promise<void> {
  try {
    const [accounts, channels] = await Promise.all([
      accountsAPI.list(1, 200),
      channelsAPI.list(1, 200),
    ])
    accountOptions.value = accounts.items || []
    channelOptions.value = channels.items || []
  } catch (error) {
    appStore.showError(localizedApiError(error, t('admin.upstreamPriceMonitor.messages.optionsFailed')))
  }
}

async function loadInitial(): Promise<void> {
  loadController?.abort()
  const controller = new AbortController()
  loadController = controller
  initialLoading.value = true
  try {
    const [config, runtimeResult, evidenceResult, runResult, modelResult] = await Promise.all([
      upstreamPriceMonitorAPI.getConfig({ signal: controller.signal }),
      upstreamPriceMonitorAPI.getRuntime({ signal: controller.signal }),
      upstreamPriceMonitorAPI.listEvidence({ signal: controller.signal }),
      upstreamPriceMonitorAPI.listRuns({ page: 1, page_size: runsPageSize.value }, { signal: controller.signal }),
      upstreamPriceMonitorAPI.listModels({ signal: controller.signal }),
    ])
    if (controller.signal.aborted) return
    assignConfig(config)
    runtime.value = runtimeResult
    evidence.value = evidenceResult.items || []
    runs.value = runResult.items || []
    runsTotal.value = runResult.total || 0
    modelCatalog.value = modelResult.items || []
    void loadOptions()
  } catch (error) {
    if ((error as { name?: string; code?: string })?.name === 'AbortError' || (error as { code?: string })?.code === 'ERR_CANCELED') return
    appStore.showError(localizedApiError(error, t('admin.upstreamPriceMonitor.messages.loadFailed')))
  } finally {
    if (loadController === controller) {
      initialLoading.value = false
      loadController = null
    }
  }
}

async function refreshOverview(): Promise<void> {
  const [runtimeResult, evidenceResult, runResult] = await Promise.all([
    upstreamPriceMonitorAPI.getRuntime(),
    upstreamPriceMonitorAPI.listEvidence(),
    upstreamPriceMonitorAPI.listRuns({ page: 1, page_size: runsPageSize.value }),
  ])
  runtime.value = runtimeResult
  evidence.value = evidenceResult.items || []
  if (runsPage.value === 1 && !runStatus.value) {
    runs.value = runResult.items || []
    runsTotal.value = runResult.total || 0
  }
}

async function refreshModelCatalog(): Promise<void> {
  const [config, models] = await Promise.all([
    upstreamPriceMonitorAPI.getConfig(),
    upstreamPriceMonitorAPI.listModels(),
  ])
  assignConfig(config)
  modelCatalog.value = models.items || []
}

async function refreshRuns(): Promise<void> {
  const result = await upstreamPriceMonitorAPI.listRuns({
    page: runsPage.value,
    page_size: runsPageSize.value,
    status: runStatus.value,
  })
  runs.value = result.items || []
  runsTotal.value = result.total || 0
}

async function refreshCurrentTab(): Promise<void> {
  refreshing.value = true
  try {
    if (activeTab.value === 'history') await refreshRuns()
    else await refreshOverview()
  } catch (error) {
    appStore.showError(localizedApiError(error, t('admin.upstreamPriceMonitor.messages.loadFailed')))
  } finally {
    refreshing.value = false
  }
}

async function openConfigPanel(): Promise<void> {
  const panel = configPanelRef.value
  if (!panel) return
  panel.open = true
  await nextTick()
  panel.scrollIntoView({ behavior: 'smooth', block: 'start' })
}

async function handleRunNow(): Promise<void> {
  creatingRun.value = true
  try {
    const run = await upstreamPriceMonitorAPI.createRun({ dry_run: true })
    runs.value = [run, ...runs.value.filter(item => item.id !== run.id)]
    runsTotal.value = Math.max(runsTotal.value + 1, runs.value.length)
    runtime.value = runtime.value ? { ...runtime.value, status: 'running', current_run_id: run.id } : runtime.value
    appStore.showSuccess(t('admin.upstreamPriceMonitor.messages.runCreated'))
    ensurePolling()
  } catch (error) {
    appStore.showError(localizedApiError(error, t('admin.upstreamPriceMonitor.messages.runFailed')))
  } finally {
    creatingRun.value = false
  }
}

function validateConfig(): string {
  if (![60, 180, 360, 720, 1440].includes(configDraft.interval_minutes)) return t('admin.upstreamPriceMonitor.config.validationInterval')
  if (configDraft.markup !== 1.2) return t('admin.upstreamPriceMonitor.config.validationMarkup')
  if (!Number.isInteger(configDraft.display_multiplier_decimals) || configDraft.display_multiplier_decimals < 0 || configDraft.display_multiplier_decimals > 6) return t('admin.upstreamPriceMonitor.config.validationDecimals')
  if (!Number.isInteger(configDraft.active_probe_max_models_per_run) || configDraft.active_probe_max_models_per_run < 1 || configDraft.active_probe_max_models_per_run > 19) return t('admin.upstreamPriceMonitor.config.validationMaxModels')
  if (!Number.isInteger(configDraft.active_probe_max_requests_per_model) || configDraft.active_probe_max_requests_per_model < 1 || configDraft.active_probe_max_requests_per_model > 7) return t('admin.upstreamPriceMonitor.config.validationMaxRequests')
  if (!Number.isFinite(configDraft.active_probe_run_budget_usd) || configDraft.active_probe_run_budget_usd <= 0 || configDraft.active_probe_run_budget_usd > 0.15) return t('admin.upstreamPriceMonitor.config.validationRunBudget')
  if (!Number.isFinite(configDraft.active_probe_daily_budget_usd) || configDraft.active_probe_daily_budget_usd <= 0 || configDraft.active_probe_daily_budget_usd > 0.40) return t('admin.upstreamPriceMonitor.config.validationDailyBudget')
  if (configDraft.active_probe_daily_budget_usd < configDraft.active_probe_run_budget_usd) return t('admin.upstreamPriceMonitor.config.validationBudgetOrder')
  if (configPayload().account_ids.length !== 1) return t('admin.upstreamPriceMonitor.config.validationSingleAccount')
  if (configDraft.enabled && configPayload().channel_ids.length === 0) return t('admin.upstreamPriceMonitor.config.validationChannels')
  if (configDraft.enabled && configPayload().domestic_models.length === 0) return t('admin.upstreamPriceMonitor.config.validationModels')
  return ''
}

async function saveConfig(): Promise<void> {
  configError.value = validateConfig()
  if (configError.value) return
  savingConfig.value = true
  try {
    const saved = await monitorStepUp.run(() => upstreamPriceMonitorAPI.updateConfig(configPayload()))
    assignConfig(saved)
    appStore.showSuccess(t('admin.upstreamPriceMonitor.config.saved'))
  } catch (error) {
    if (isStepUpCancelled(error)) return
    appStore.showError(localizedApiError(error, t('admin.upstreamPriceMonitor.messages.saveFailed')))
  } finally {
    savingConfig.value = false
  }
}

async function updateModelStatus(model: string, status: UpstreamPriceModelStatus): Promise<void> {
  updatingModel.value = model
  try {
    await upstreamPriceMonitorAPI.updateModelStatus(model, status)
    await refreshModelCatalog()
    appStore.showSuccess(t('admin.upstreamPriceMonitor.config.modelUpdated'))
  } catch (error) {
    appStore.showError(localizedApiError(error, t('admin.upstreamPriceMonitor.messages.modelUpdateFailed')))
  } finally {
    updatingModel.value = ''
  }
}

async function discoverModels(): Promise<void> {
  discoveringModels.value = true
  try {
    const result = await upstreamPriceMonitorAPI.discoverModels()
    modelCatalog.value = result.items || []
    const config = await upstreamPriceMonitorAPI.getConfig()
    assignConfig(config)
    appStore.showSuccess(t('admin.upstreamPriceMonitor.config.modelsDiscovered'))
  } catch (error) {
    appStore.showError(localizedApiError(error, t('admin.upstreamPriceMonitor.messages.modelDiscoveryFailed')))
  } finally {
    discoveringModels.value = false
  }
}

function requestRunAction(type: PendingRunAction['type'], run: UpstreamPriceMonitorRun): void {
  pendingRunAction.value = { type, run }
}

async function confirmRunAction(): Promise<void> {
  const action = pendingRunAction.value
  pendingRunAction.value = null
  const snapshotHash = action?.run.snapshot_hash
  if (!action || !snapshotHash) return
  try {
    if (action.type === 'rollback') {
      await monitorStepUp.run(() => upstreamPriceMonitorAPI.rollbackRun(action.run.id, { snapshot_hash: snapshotHash }))
      appStore.showSuccess(t('admin.upstreamPriceMonitor.messages.rollbackSuccess'))
    } else {
      await monitorStepUp.run(() => upstreamPriceMonitorAPI.applyRun(action.run.id, { snapshot_hash: snapshotHash }))
      appStore.showSuccess(t('admin.upstreamPriceMonitor.messages.applySuccess'))
    }
    await refreshOverview()
    if (activeTab.value === 'history') await refreshRuns()
  } catch (error) {
    if (isStepUpCancelled(error)) return
    const fallback = action.type === 'rollback'
      ? t('admin.upstreamPriceMonitor.messages.rollbackFailed')
      : t('admin.upstreamPriceMonitor.messages.applyFailed')
    appStore.showError(localizedApiError(error, fallback))
  }
}

function canApply(run: UpstreamPriceMonitorRun): boolean {
  return configDraft.mode === 'review' && run.status === 'completed' && Boolean(run.snapshot_hash) && !run.applied_at
}

function canRollback(run: UpstreamPriceMonitorRun): boolean {
  return configDraft.mode !== 'observe' && Boolean(run.rollback_available && run.snapshot_hash && run.applied_at)
}

function handleHistoryFilter(): void {
  runsPage.value = 1
  void refreshCurrentTab()
}

function handleRunsPage(page: number): void {
  runsPage.value = page
  void refreshCurrentTab()
}

function handleRunsPageSize(pageSize: number): void {
  runsPageSize.value = pageSize
  runsPage.value = 1
  void refreshCurrentTab()
}

function statusLabel(status: string): string {
  return knownStatuses.has(status)
    ? t(`admin.upstreamPriceMonitor.status.${status}`)
    : t('admin.upstreamPriceMonitor.status.unknown')
}

function modeLabel(mode: string): string {
  return knownModes.has(mode)
    ? t(`admin.upstreamPriceMonitor.mode.${mode}`)
    : t('admin.upstreamPriceMonitor.mode.unknown')
}

function modeShortHint(mode: string): string {
  if (mode === 'auto_apply') return t('admin.upstreamPriceMonitor.mode.shortAuto')
  if (mode === 'review') return t('admin.upstreamPriceMonitor.mode.shortReview')
  return t('admin.upstreamPriceMonitor.mode.shortObserve')
}

function modeLongHint(mode: string): string {
  if (mode === 'auto_apply') return t('admin.upstreamPriceMonitor.overview.modeHintAuto')
  if (mode === 'review') return t('admin.upstreamPriceMonitor.overview.modeHintReview')
  return t('admin.upstreamPriceMonitor.overview.modeHintObserve')
}

function intervalLabel(minutes: number): string {
  if (minutes === 60) return t('admin.upstreamPriceMonitor.interval.hour', { count: 1 })
  if (minutes % 60 === 0) return t('admin.upstreamPriceMonitor.interval.hour', { count: minutes / 60 })
  return t('admin.upstreamPriceMonitor.interval.minute', { count: minutes })
}

function triggerLabel(trigger: string): string {
  return knownTriggers.has(trigger)
    ? t(`admin.upstreamPriceMonitor.history.${trigger}`)
    : t('admin.upstreamPriceMonitor.history.unknownTrigger')
}

function runSummaryTitle(run: UpstreamPriceMonitorRun): string {
  if (run.status === 'running') return t('admin.upstreamPriceMonitor.history.summaryRunning')
  if (run.status === 'failed') return t('admin.upstreamPriceMonitor.history.summaryFailed')
  if (run.status === 'partial') return t('admin.upstreamPriceMonitor.history.summaryPartial')
  const applied = Number(run.summary?.applied_models || 0)
  if (applied > 0 || run.applied_at) return t('admin.upstreamPriceMonitor.history.summaryApplied', { count: applied || run.matched_models })
  return t('admin.upstreamPriceMonitor.history.summaryCompleted', { count: run.matched_models })
}

function runStatusClass(status: string): string {
  if (status === 'completed' || status === 'idle') return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-500/15 dark:text-emerald-300'
  if (status === 'running') return 'bg-blue-100 text-blue-700 dark:bg-blue-500/15 dark:text-blue-300'
  if (status === 'failed') return 'bg-red-100 text-red-700 dark:bg-red-500/15 dark:text-red-300'
  if (status === 'degraded' || status === 'partial') return 'bg-amber-100 text-amber-700 dark:bg-amber-500/15 dark:text-amber-300'
  return 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-dark-200'
}

function evidenceStatusClass(status: UpstreamPriceEvidenceStatus): string {
  if (status === 'trusted') return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-500/15 dark:text-emerald-300'
  if (status === 'pending') return 'bg-blue-100 text-blue-700 dark:bg-blue-500/15 dark:text-blue-300'
  if (status === 'mismatch') return 'bg-red-100 text-red-700 dark:bg-red-500/15 dark:text-red-300'
  return 'bg-amber-100 text-amber-700 dark:bg-amber-500/15 dark:text-amber-300'
}

function isEvidenceMismatch(item: UpstreamPriceEvidence): boolean {
  return item.status === 'mismatch' || ['mismatch', 'open', 'external_traffic'].includes(item.reconciliation_status || '')
}

function reconciliationLabel(item: UpstreamPriceEvidence): string {
  if (item.reconciliation_status) {
    return knownReconciliationStatuses.has(item.reconciliation_status)
      ? t(`admin.upstreamPriceMonitor.reconciliation.${item.reconciliation_status}`)
      : t('admin.upstreamPriceMonitor.reconciliation.unknown')
  }
  return statusLabel(item.status === 'trusted' ? 'trusted' : item.status)
}

function evidenceObservedPrices(item: UpstreamPriceEvidence) {
  return item.prices || {}
}

interface PriceField {
  key: UpstreamPriceDimension
  label: string
  current?: number | null
  display?: number | null
  observed?: number | null
  suggested?: number | null
  status: UpstreamPriceDimensionStatus
}

function resolvedDimensionStatus(
  item: UpstreamPriceEvidence,
  key: UpstreamPriceDimension,
  observed?: number | null,
): UpstreamPriceDimensionStatus {
  const explicit = item.dimension_statuses?.[key]
  if (explicit) return explicit
  if (observed !== null && observed !== undefined && Number.isFinite(observed)) return 'observed'
  if (item.status === 'pending') return 'pending'
  return 'unobserved'
}

function priceFields(item: UpstreamPriceEvidence): PriceField[] {
  const current = item.current_prices || evidenceObservedPrices(item)
  const suggested = item.suggested_prices || {}
  const observed = evidenceObservedPrices(item)
  const display = item.display_prices_current || {}
  if (item.billing_mode === 'per_request') {
    return [
      { key: 'per_request_lte_256k', label: t('admin.upstreamPriceMonitor.overview.priceRequestLow'), current: current.per_request_lte_256k, display: display.per_request_lte_256k, observed: observed.per_request_lte_256k, suggested: suggested.per_request_lte_256k, status: resolvedDimensionStatus(item, 'per_request_lte_256k', observed.per_request_lte_256k) },
      { key: 'per_request_256k_512k', label: t('admin.upstreamPriceMonitor.overview.priceRequestMedium'), current: current.per_request_256k_512k, display: display.per_request_256k_512k, observed: observed.per_request_256k_512k, suggested: suggested.per_request_256k_512k, status: resolvedDimensionStatus(item, 'per_request_256k_512k', observed.per_request_256k_512k) },
      { key: 'per_request_gt_512k', label: t('admin.upstreamPriceMonitor.overview.priceRequestHigh'), current: current.per_request_gt_512k, display: display.per_request_gt_512k, observed: observed.per_request_gt_512k, suggested: suggested.per_request_gt_512k, status: resolvedDimensionStatus(item, 'per_request_gt_512k', observed.per_request_gt_512k) },
    ]
  }
  return [
    { key: 'fixed_per_request', label: t('admin.upstreamPriceMonitor.overview.priceFixedRequest'), current: current.fixed_per_request, display: display.fixed_per_request, observed: observed.fixed_per_request, suggested: suggested.fixed_per_request, status: resolvedDimensionStatus(item, 'fixed_per_request', observed.fixed_per_request) },
    { key: 'input', label: t('admin.upstreamPriceMonitor.overview.priceInput'), current: current.input_per_million, display: display.input_per_million, observed: observed.input_per_million, suggested: suggested.input_per_million, status: resolvedDimensionStatus(item, 'input', observed.input_per_million) },
    { key: 'output', label: t('admin.upstreamPriceMonitor.overview.priceOutput'), current: current.output_per_million, display: display.output_per_million, observed: observed.output_per_million, suggested: suggested.output_per_million, status: resolvedDimensionStatus(item, 'output', observed.output_per_million) },
    { key: 'cache_write', label: t('admin.upstreamPriceMonitor.overview.priceCacheWrite'), current: current.cache_write_per_million, display: display.cache_write_per_million, observed: observed.cache_write_per_million, suggested: suggested.cache_write_per_million, status: resolvedDimensionStatus(item, 'cache_write', observed.cache_write_per_million) },
    { key: 'cache_read', label: t('admin.upstreamPriceMonitor.overview.priceCacheRead'), current: current.cache_read_per_million, display: display.cache_read_per_million, observed: observed.cache_read_per_million, suggested: suggested.cache_read_per_million, status: resolvedDimensionStatus(item, 'cache_read', observed.cache_read_per_million) },
  ]
}

function visiblePriceFields(item: UpstreamPriceEvidence): PriceField[] {
  const populated = priceFields(item).filter(field => [field.current, field.display, field.observed, field.suggested].some(value => value !== null && value !== undefined))
  return populated.length ? populated : priceFields(item).filter(field => field.key !== 'fixed_per_request')
}

function hasDisplayPrices(item: UpstreamPriceEvidence): boolean {
  return visiblePriceFields(item).some(field => field.display !== null && field.display !== undefined)
}

function dimensionDotClass(status: UpstreamPriceDimensionStatus): string {
  if (status === 'observed') return 'bg-emerald-500'
  if (status === 'pending') return 'bg-blue-500'
  if (status === 'failed') return 'bg-amber-500'
  return 'bg-gray-300 dark:bg-dark-500'
}

function evidenceNeedsApply(item: UpstreamPriceEvidence): boolean {
  return visiblePriceFields(item).some(field => priceChanged(field.current, field.suggested) || (field.display !== null && field.display !== undefined && priceChanged(field.display, field.suggested)))
}

function evidenceWasApplied(item: UpstreamPriceEvidence): boolean {
  if (item.status !== 'trusted' || !item.observed_at) return false
  const observedAt = Date.parse(item.observed_at)
  if (!Number.isFinite(observedAt)) return false
  return runs.value.some(run => {
    if (!run.applied_at) return false
    const startedAt = Date.parse(run.started_at)
    const finishedAt = Date.parse(run.finished_at || run.applied_at)
    if (!Number.isFinite(startedAt) || !Number.isFinite(finishedAt)) return false
    return observedAt >= startedAt - 5_000 && observedAt <= finishedAt + 5 * 60_000
  })
}

function applyStateLabel(item: UpstreamPriceEvidence): string {
  if (item.status !== 'trusted') return t('admin.upstreamPriceMonitor.applyState.unavailable')
  if (evidenceWasApplied(item)) return t('admin.upstreamPriceMonitor.applyState.appliedRun')
  if (!evidenceNeedsApply(item)) return t('admin.upstreamPriceMonitor.applyState.applied')
  if (configDraft.mode === 'observe') return t('admin.upstreamPriceMonitor.applyState.observedOnly')
  if (configDraft.mode === 'review') return t('admin.upstreamPriceMonitor.applyState.awaitingReview')
  return t('admin.upstreamPriceMonitor.applyState.awaitingAuto')
}

function applyStateClass(item: UpstreamPriceEvidence): string {
  if (item.status !== 'trusted') return 'bg-gray-100 text-gray-600 dark:bg-dark-700 dark:text-dark-200'
  if (evidenceWasApplied(item) || !evidenceNeedsApply(item)) return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-500/15 dark:text-emerald-300'
  if (configDraft.mode === 'observe') return 'bg-blue-100 text-blue-700 dark:bg-blue-500/15 dark:text-blue-300'
  return 'bg-amber-100 text-amber-700 dark:bg-amber-500/15 dark:text-amber-300'
}

function localizedMonitorError(value?: string | null): string {
  const error = (value || '').toLowerCase()
  if (/budget|cost limit|spend limit/.test(error)) return t('admin.upstreamPriceMonitor.error.budget')
  if (/timeout|deadline|timed out|cancel/.test(error)) return t('admin.upstreamPriceMonitor.error.timeout')
  if (/429|rate.?limit|too many request/.test(error)) return t('admin.upstreamPriceMonitor.error.rateLimit')
  if (/401|403|unauthor|forbidden|invalid.*key|api.?key/.test(error)) return t('admin.upstreamPriceMonitor.error.credential')
  if (/external.?traffic|ledger|reconcil|mismatch|mixed.?context/.test(error)) return t('admin.upstreamPriceMonitor.error.contaminated')
  if (/upstream|bad gateway|502|503|504/.test(error)) return t('admin.upstreamPriceMonitor.error.upstream')
  return t('admin.upstreamPriceMonitor.error.generic')
}

function localizedApiError(error: unknown, fallback: string): string {
  const message = extractApiErrorMessage(error, fallback)
  if (message === fallback || /[\u3400-\u9fff]/.test(message)) return message
  return localizedMonitorError(message)
}

function formatPrice(value?: number | null): string {
  if (value === null || value === undefined || !Number.isFinite(value)) return '—'
  return `$${value.toFixed(value !== 0 && Math.abs(value) < 0.01 ? 8 : 6).replace(/\.?0+$/, '')}`
}

function formatMoney(value?: number | null): string {
  if (value === null || value === undefined || !Number.isFinite(value)) return '$0.00'
  return `$${value.toFixed(value > 0 && value < 0.01 ? 6 : 2)}`
}

function priceChanged(current?: number | null, suggested?: number | null): boolean {
  if (suggested === null || suggested === undefined || !Number.isFinite(suggested)) return false
  if (current === null || current === undefined || !Number.isFinite(current)) return true
  return Math.abs(current - suggested) > 1e-12
}

function formatTimestamp(value: string | null | undefined, kind: 'last' | 'next'): string {
  if (!value) return kind === 'last' ? t('admin.upstreamPriceMonitor.runtime.noRun') : t('admin.upstreamPriceMonitor.runtime.notScheduled')
  return formatDateTime(value)
}

async function pollActiveRun(): Promise<void> {
  if (document.visibilityState !== 'visible') return
  const hasRunning = runtime.value?.status === 'running' || runs.value.some(run => run.status === 'running')
  if (!hasRunning) {
    stopPolling()
    return
  }
  try {
    await refreshOverview()
    if (activeTab.value === 'history') await refreshRuns()
  } catch {
    // Polling is best-effort. The next interval or manual refresh surfaces state.
  }
}

function ensurePolling(): void {
  if (pollTimer || document.visibilityState !== 'visible') return
  pollTimer = setInterval(() => void pollActiveRun(), 2000)
}

function stopPolling(): void {
  if (!pollTimer) return
  clearInterval(pollTimer)
  pollTimer = null
}

function handleVisibilityChange(): void {
  if (document.visibilityState === 'visible') {
    const hasRunning = runtime.value?.status === 'running' || runs.value.some(run => run.status === 'running')
    if (hasRunning) {
      void refreshCurrentTab()
      ensurePolling()
    }
  } else stopPolling()
}

function handleBeforeUnload(event: BeforeUnloadEvent): void {
  if (!configDirty.value) return
  event.preventDefault()
  event.returnValue = ''
}

watch(activeTab, tab => {
  if (tab === 'history') void refreshRuns()
})

watch(
  () => [runtime.value?.status, runs.value.map(run => run.status).join(',')],
  () => {
    const hasRunning = runtime.value?.status === 'running' || runs.value.some(run => run.status === 'running')
    if (hasRunning) ensurePolling()
    else stopPolling()
  },
)

onMounted(() => {
  document.addEventListener('visibilitychange', handleVisibilityChange)
  window.addEventListener('beforeunload', handleBeforeUnload)
  void loadInitial()
})

onUnmounted(() => {
  loadController?.abort()
  stopPolling()
  document.removeEventListener('visibilitychange', handleVisibilityChange)
  window.removeEventListener('beforeunload', handleBeforeUnload)
})
</script>

<style scoped>
.monitor-panel {
  @apply rounded-2xl border border-gray-200 bg-white shadow-sm dark:border-dark-700 dark:bg-dark-800;
}

.monitor-stat-card {
  @apply flex min-h-[118px] flex-col rounded-2xl border border-gray-200 bg-white p-4 shadow-sm dark:border-dark-700 dark:bg-dark-800;
}

.monitor-stat-label {
  @apply text-xs font-semibold uppercase tracking-wide text-gray-500 dark:text-dark-400;
}

.monitor-stat-value {
  @apply mt-3 text-xl font-black text-gray-900 dark:text-white;
}

.monitor-chip {
  @apply inline-flex rounded-md bg-gray-100 px-2 py-0.5 text-xs font-medium text-gray-600 dark:bg-dark-700 dark:text-dark-200;
}

.price-summary-column {
  @apply min-w-0 rounded-xl border border-gray-100 bg-gray-50/60 p-3 dark:border-dark-700 dark:bg-dark-900/30;
}

.price-summary-title {
  @apply mb-2 text-[11px] font-bold uppercase tracking-wide text-gray-500 dark:text-dark-400;
}

.price-summary-row {
  @apply mt-1.5 flex items-center justify-between gap-3 text-xs text-gray-500 dark:text-dark-300;
}

.price-summary-row strong {
  @apply whitespace-nowrap font-mono font-semibold text-gray-800 dark:text-gray-100;
}

.config-toggle-row {
  @apply flex items-start justify-between gap-4 rounded-2xl border border-gray-200 bg-gray-50/60 p-4 dark:border-dark-700 dark:bg-dark-900/30;
}

.config-label {
  @apply text-sm font-semibold text-gray-900 dark:text-white;
}

.config-hint {
  @apply block text-xs leading-5 text-gray-500 dark:text-dark-400;
}
</style>
