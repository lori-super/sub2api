<template>
  <div class="card" data-testid="media-bridge-settings">
    <div class="border-b border-gray-100 px-6 py-4 dark:border-dark-700">
      <div class="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 class="text-lg font-semibold text-gray-900 dark:text-white">
            {{ t("admin.settings.mediaBridge.title") }}
          </h2>
          <p class="mt-1 text-sm text-gray-500 dark:text-gray-400">
            {{ t("admin.settings.mediaBridge.description") }}
          </p>
        </div>
        <span
          v-if="!loading && !loadFailed"
          class="rounded-full bg-gray-100 px-2.5 py-1 text-xs text-gray-600 dark:bg-dark-700 dark:text-gray-300"
        >
          {{ t("admin.settings.mediaBridge.version", { version: form.version }) }}
        </span>
      </div>
    </div>

    <div v-if="loading" class="flex items-center gap-2 p-6 text-gray-500">
      <div class="h-4 w-4 animate-spin rounded-full border-b-2 border-primary-600"></div>
      {{ t("common.loading") }}
    </div>

    <div v-else-if="loadFailed" class="space-y-3 p-6">
      <p class="text-sm text-red-600 dark:text-red-400">
        {{ t("admin.settings.mediaBridge.loadFailed") }}
      </p>
      <button type="button" class="btn btn-secondary btn-sm" @click="loadSettings">
        {{ t("admin.settings.mediaBridge.retry") }}
      </button>
    </div>

    <div v-else class="space-y-7 p-6">
      <div class="rounded-lg border border-blue-200 bg-blue-50 p-4 dark:border-blue-800 dark:bg-blue-900/20">
        <div class="flex items-start gap-3">
          <Icon name="infoCircle" size="md" class="mt-0.5 flex-shrink-0 text-blue-500" />
          <div class="space-y-1 text-sm text-blue-700 dark:text-blue-300">
            <p class="font-medium">{{ t("admin.settings.mediaBridge.hotApplyTitle") }}</p>
            <p>{{ t("admin.settings.mediaBridge.hotApplyDescription") }}</p>
          </div>
        </div>
      </div>

      <section class="space-y-4">
        <SectionTitle
          :title="t('admin.settings.mediaBridge.runtime.title')"
          :description="t('admin.settings.mediaBridge.runtime.description')"
        />
        <div class="grid gap-4 md:grid-cols-2">
          <div>
            <label class="field-label">{{ t("admin.settings.mediaBridge.runtime.mode") }}</label>
            <select v-model="form.mode" data-testid="media-bridge-mode" class="input w-full">
              <option v-for="mode in MEDIA_BRIDGE_MODES" :key="mode" :value="mode">
                {{ t(`admin.settings.mediaBridge.modes.${mode}.label`) }}
              </option>
            </select>
            <p class="field-hint">
              {{ t(`admin.settings.mediaBridge.modes.${form.mode}.description`) }}
            </p>
          </div>
          <div>
            <label class="field-label">{{ t("admin.settings.mediaBridge.runtime.canaryPercent") }}</label>
            <input
              v-model.number="form.canary_percent"
              data-testid="media-bridge-canary-percent"
              type="number"
              min="0"
              max="100"
              class="input w-full"
              :disabled="form.mode !== 'canary'"
            />
            <p class="field-hint">{{ t("admin.settings.mediaBridge.runtime.canaryPercentHint") }}</p>
          </div>
        </div>
        <p
          v-if="form.mode === 'drain'"
          class="rounded-lg bg-amber-50 px-3 py-2 text-sm text-amber-700 dark:bg-amber-900/20 dark:text-amber-300"
        >
          {{ t("admin.settings.mediaBridge.runtime.drainWarning") }}
        </p>
      </section>

      <section class="space-y-4 border-t border-gray-100 pt-6 dark:border-dark-700">
        <SectionTitle
          :title="t('admin.settings.mediaBridge.scope.title')"
          :description="t('admin.settings.mediaBridge.scope.description')"
        />
        <div class="grid gap-4 md:grid-cols-2">
          <div class="space-y-3 rounded-lg border border-gray-200 p-4 md:col-span-2 dark:border-dark-600">
            <div class="grid gap-4 md:grid-cols-2">
              <div>
                <label class="field-label">{{ t("admin.settings.mediaBridge.scope.ingressProtocols") }}</label>
                <div data-testid="media-bridge-ingress-protocols" class="flex flex-wrap gap-3">
                  <label
                    v-for="protocol in MEDIA_BRIDGE_INGRESS_PROTOCOLS"
                    :key="protocol"
                    class="flex cursor-pointer items-center gap-2 rounded-lg border border-gray-200 px-3 py-2 text-sm text-gray-700 dark:border-dark-600 dark:text-gray-300"
                  >
                    <input
                      v-model="form.scope.ingress_protocols"
                      type="checkbox"
                      :value="protocol"
                      :data-testid="`media-bridge-ingress-${protocol}`"
                      class="h-4 w-4 rounded border-gray-300 text-primary-600"
                    />
                    <span>{{ t(`admin.settings.mediaBridge.scope.ingress.${protocol}`) }}</span>
                  </label>
                </div>
                <p class="field-hint">{{ t("admin.settings.mediaBridge.scope.ingressHint") }}</p>
              </div>
              <div>
                <label class="field-label">{{ t("admin.settings.mediaBridge.scope.finalEgress") }}</label>
                <div
                  data-testid="media-bridge-final-egress"
                  class="rounded-lg bg-gray-50 px-3 py-2 text-sm font-medium text-gray-800 dark:bg-dark-800 dark:text-gray-200"
                >
                  {{ t("admin.settings.mediaBridge.scope.finalEgressChat") }}
                </div>
                <p class="field-hint">{{ t("admin.settings.mediaBridge.scope.finalEgressHint") }}</p>
              </div>
            </div>
            <p class="rounded-lg bg-amber-50 px-3 py-2 text-xs text-amber-700 dark:bg-amber-900/20 dark:text-amber-300">
              {{ t("admin.settings.mediaBridge.scope.vendorExtensionNotice") }}
            </p>
          </div>
          <div>
            <label class="field-label">{{ t("admin.settings.mediaBridge.scope.models") }}</label>
            <textarea
              v-model="scopeModelsInput"
              data-testid="media-bridge-scope-models"
              rows="3"
              class="input w-full resize-y"
              :placeholder="t('admin.settings.mediaBridge.scope.modelsPlaceholder')"
            ></textarea>
            <p class="field-hint">{{ t("admin.settings.mediaBridge.scope.listHint") }}</p>
          </div>
          <div>
            <label class="field-label">{{ t("admin.settings.mediaBridge.scope.accountIds") }}</label>
            <textarea
              v-model="scopeAccountIDsInput"
              rows="3"
              class="input w-full resize-y"
              placeholder="1, 2, 3"
            ></textarea>
            <p class="field-hint">{{ t("admin.settings.mediaBridge.scope.accountIdsHint") }}</p>
          </div>
        </div>
      </section>

      <section class="space-y-4 border-t border-gray-100 pt-6 dark:border-dark-700">
        <SectionTitle
          :title="t('admin.settings.mediaBridge.capacity.title')"
          :description="t('admin.settings.mediaBridge.capacity.description')"
        />
        <div class="rounded-lg bg-gray-50 px-3 py-2 text-xs text-gray-600 dark:bg-dark-800 dark:text-gray-300">
          {{ t("admin.settings.mediaBridge.zeroValueHint") }}
        </div>
        <div class="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
          <div v-for="field in capacityFields" :key="field.key">
            <label class="field-label">{{ t(field.label) }}</label>
            <input
              v-model.number="form.capacity[field.key]"
              type="number"
              min="0"
              :max="field.max"
              step="1"
              class="input w-full"
            />
            <p class="field-hint">
              {{ field.bytes ? formatBytes(form.capacity[field.key], field.rate) : t(field.hint) }}
            </p>
          </div>
        </div>

        <div class="space-y-3 rounded-lg border border-gray-200 p-4 dark:border-dark-600">
          <div class="flex items-center justify-between gap-3">
            <div>
              <h4 class="text-sm font-medium text-gray-900 dark:text-white">
                {{ t("admin.settings.mediaBridge.capacity.tenantOverrides") }}
              </h4>
              <p class="field-hint">{{ t("admin.settings.mediaBridge.capacity.tenantOverridesHint") }}</p>
            </div>
            <button type="button" data-testid="media-bridge-add-tenant" class="btn btn-secondary btn-sm" @click="addTenantOverride">
              {{ t("admin.settings.mediaBridge.capacity.addTenant") }}
            </button>
          </div>
          <div v-if="form.capacity.tenant_overrides.length === 0" class="text-sm text-gray-500 dark:text-gray-400">
            {{ t("admin.settings.mediaBridge.capacity.noTenantOverrides") }}
          </div>
          <div v-else class="overflow-x-auto">
            <table class="min-w-[940px] w-full text-left text-sm">
              <thead class="text-xs text-gray-500 dark:text-gray-400">
                <tr>
                  <th class="pb-2 pr-2">{{ t("admin.settings.mediaBridge.capacity.tenantId") }}</th>
                  <th class="pb-2 pr-2">{{ t("admin.settings.mediaBridge.capacity.weight") }}</th>
                  <th class="pb-2 pr-2">{{ t("admin.settings.mediaBridge.capacity.maxInflightRequests") }}</th>
                  <th class="pb-2 pr-2">{{ t("admin.settings.mediaBridge.capacity.maxInflightBytes") }}</th>
                  <th class="pb-2 pr-2">{{ t("admin.settings.mediaBridge.capacity.maxBandwidth") }}</th>
                  <th class="pb-2 text-right">{{ t("common.actions") }}</th>
                </tr>
              </thead>
              <tbody>
                <tr v-for="(override, index) in form.capacity.tenant_overrides" :key="index">
                  <td class="pr-2 pt-2"><input v-model.number="override.tenant_id" type="number" min="1" class="input w-28" /></td>
                  <td class="pr-2 pt-2"><input v-model.number="override.weight" type="number" min="1" class="input w-24" /></td>
                  <td class="pr-2 pt-2"><input v-model.number="override.max_inflight_requests" type="number" min="0" class="input w-32" /></td>
                  <td class="pr-2 pt-2"><input v-model.number="override.max_inflight_decoded_bytes" type="number" min="0" class="input w-44" /></td>
                  <td class="pr-2 pt-2"><input v-model.number="override.max_bandwidth_bytes_per_second" type="number" min="0" class="input w-44" /></td>
                  <td class="pt-2 text-right">
                    <button type="button" class="btn btn-secondary btn-sm text-red-600 dark:text-red-400" @click="removeTenantOverride(index)">
                      {{ t("common.delete") }}
                    </button>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      </section>

      <section class="space-y-4 border-t border-gray-100 pt-6 dark:border-dark-700">
        <SectionTitle
          :title="t('admin.settings.mediaBridge.protection.title')"
          :description="t('admin.settings.mediaBridge.protection.description')"
        />
        <div class="rounded-lg bg-gray-50 px-3 py-2 text-xs text-gray-600 dark:bg-dark-800 dark:text-gray-300">
          {{ t("admin.settings.mediaBridge.protection.zeroValueHint") }}
        </div>
        <div class="grid gap-4 md:grid-cols-2 xl:grid-cols-4">
          <div v-for="field in protectionFields" :key="field.key">
            <label class="field-label">{{ t(field.label) }}</label>
            <input
              v-model.number="form.protection[field.key]"
              type="number"
              min="0"
              :max="field.max"
              step="1"
              class="input w-full"
            />
            <p v-if="field.bytes" class="field-hint">{{ formatBytes(form.protection[field.key]) }}</p>
          </div>
        </div>
      </section>

      <section class="space-y-4 border-t border-gray-100 pt-6 dark:border-dark-700">
        <SectionTitle
          :title="t('admin.settings.mediaBridge.filePolicy.title')"
          :description="t('admin.settings.mediaBridge.filePolicy.description')"
        />
        <div>
          <label class="field-label">{{ t("admin.settings.mediaBridge.filePolicy.allowedMimeTypes") }}</label>
          <textarea
            v-model="allowedMimeTypesInput"
            rows="2"
            class="input w-full resize-y"
            placeholder="video/mp4"
          ></textarea>
          <p class="field-hint">{{ t("admin.settings.mediaBridge.scope.listHint") }}</p>
        </div>
        <div class="grid gap-4 md:grid-cols-3">
          <div v-for="field in filePolicyFields" :key="field.key">
            <label class="field-label">{{ t(field.label) }}</label>
            <input v-model.number="form.file_policy[field.key]" type="number" min="0" step="1" class="input w-full" />
            <p class="field-hint">{{ field.bytes ? formatBytes(form.file_policy[field.key]) : t(field.hint) }}</p>
          </div>
        </div>
        <label class="flex items-start gap-3">
          <input v-model="form.file_policy.deduplicate_within_request" type="checkbox" class="mt-1 h-4 w-4 rounded border-gray-300 text-primary-600" />
          <span>
            <span class="block text-sm font-medium text-gray-900 dark:text-white">{{ t("admin.settings.mediaBridge.filePolicy.deduplicate") }}</span>
            <span class="field-hint">{{ t("admin.settings.mediaBridge.filePolicy.deduplicateHint") }}</span>
          </span>
        </label>
      </section>

      <section class="space-y-4 border-t border-gray-100 pt-6 dark:border-dark-700">
        <SectionTitle
          :title="t('admin.settings.mediaBridge.retention.title')"
          :description="t('admin.settings.mediaBridge.retention.description')"
        />
        <div class="grid gap-4 md:grid-cols-2">
          <div v-for="field in retentionFields" :key="field.key">
            <label class="field-label">{{ t(field.label) }}</label>
            <input v-model.number="form.retention[field.key]" type="number" min="0" :max="field.max" step="1" class="input w-full" />
            <p class="field-hint">{{ t(field.hint) }}</p>
          </div>
        </div>
      </section>

      <section class="space-y-4 border-t border-gray-100 pt-6 dark:border-dark-700">
        <div class="flex flex-wrap items-start justify-between gap-3">
          <SectionTitle
            :title="t('admin.settings.mediaBridge.storage.title')"
            :description="t('admin.settings.mediaBridge.storage.description')"
          />
          <span
            data-testid="media-bridge-storage-status"
            class="rounded-full px-2.5 py-1 text-xs"
            :class="storageDisplayReady
              ? 'bg-green-100 text-green-700 dark:bg-green-900/30 dark:text-green-300'
              : 'bg-amber-100 text-amber-700 dark:bg-amber-900/30 dark:text-amber-300'"
          >
            {{ t(storageDisplayReady
              ? "admin.settings.mediaBridge.storage.ready"
              : "admin.settings.mediaBridge.storage.notReady") }}
          </span>
        </div>
        <div class="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
          <div>
            <label class="field-label">{{ t("admin.settings.mediaBridge.storage.provider") }}</label>
            <select v-model="form.storage.provider" class="input w-full">
              <option value="r2">Cloudflare R2</option>
            </select>
          </div>
          <div class="xl:col-span-2">
            <label class="field-label">{{ t("admin.settings.mediaBridge.storage.endpoint") }}</label>
            <input v-model.trim="form.storage.endpoint" type="url" class="input w-full" placeholder="https://account.r2.cloudflarestorage.com" />
          </div>
          <div>
            <label class="field-label">{{ t("admin.settings.mediaBridge.storage.region") }}</label>
            <input v-model.trim="form.storage.region" type="text" class="input w-full" placeholder="auto" />
          </div>
          <div>
            <label class="field-label">{{ t("admin.settings.mediaBridge.storage.bucket") }}</label>
            <input v-model.trim="form.storage.bucket" type="text" class="input w-full" placeholder="worldcodes-sub2api-media-bridge" />
          </div>
          <div>
            <label class="field-label">{{ t("admin.settings.mediaBridge.storage.objectPrefix") }}</label>
            <input v-model.trim="form.storage.object_prefix" data-testid="media-bridge-object-prefix" type="text" class="input w-full" placeholder="media-bridge" />
          </div>
          <div>
            <label class="field-label">{{ t("admin.settings.mediaBridge.storage.accessKeyId") }}</label>
            <input
              v-model.trim="form.storage.access_key_id"
              data-testid="media-bridge-access-key-id"
              type="text"
              autocomplete="off"
              class="input w-full"
            />
          </div>
          <div>
            <label class="field-label">{{ t("admin.settings.mediaBridge.storage.secretAccessKey") }}</label>
            <input
              v-model="form.storage.secret_access_key"
              data-testid="media-bridge-secret-access-key"
              type="password"
              autocomplete="new-password"
              class="input w-full"
              :placeholder="form.storage.secret_configured
                ? t('admin.settings.mediaBridge.storage.secretConfiguredPlaceholder')
                : t('admin.settings.mediaBridge.storage.secretPlaceholder')"
            />
            <p class="field-hint">{{ t("admin.settings.mediaBridge.storage.secretHint") }}</p>
          </div>
          <label class="flex items-start gap-3 self-end pb-2">
            <input
              v-model="form.storage.force_path_style"
              data-testid="media-bridge-force-path-style"
              type="checkbox"
              class="mt-1 h-4 w-4 rounded border-gray-300 text-primary-600"
            />
            <span>
              <span class="block text-sm font-medium text-gray-900 dark:text-white">{{ t("admin.settings.mediaBridge.storage.forcePathStyle") }}</span>
              <span class="field-hint">{{ t("admin.settings.mediaBridge.storage.forcePathStyleHint") }}</span>
            </span>
          </label>
        </div>
        <div class="flex flex-wrap items-center justify-between gap-3 rounded-lg bg-gray-50 px-3 py-2 dark:bg-dark-800">
          <p class="text-xs text-gray-600 dark:text-gray-300">
            {{ t("admin.settings.mediaBridge.storage.credentialsHint") }}
          </p>
          <button
            type="button"
            data-testid="media-bridge-test-storage"
            class="btn btn-secondary btn-sm"
            :disabled="testingStorage || Boolean(storageValidationError)"
            @click="testStorage"
          >
            {{ testingStorage
              ? t("admin.settings.mediaBridge.storage.testing")
              : t("admin.settings.mediaBridge.storage.test") }}
          </button>
        </div>
      </section>

      <p v-if="validationError" data-testid="media-bridge-validation-error" class="text-sm text-red-600 dark:text-red-400">
        {{ validationError }}
      </p>

      <div class="flex justify-end border-t border-gray-100 pt-5 dark:border-dark-700">
        <button
          type="button"
          data-testid="media-bridge-save"
          class="btn btn-primary btn-sm"
          :disabled="saving || Boolean(validationError)"
          @click="saveSettings"
        >
          {{ saving ? t("common.saving") : t("common.save") }}
        </button>
      </div>
    </div>
    <TotpStepUpDialog :controller="mediaBridgeStepUp" />
  </div>
</template>

<script setup lang="ts">
import { computed, defineComponent, h, onMounted, reactive, ref } from "vue";
import { useI18n } from "vue-i18n";
import { adminAPI } from "@/api";
import {
  MEDIA_BRIDGE_INGRESS_PROTOCOLS,
  MEDIA_BRIDGE_K3_VIDEO_EGRESS_PROTOCOL,
  MEDIA_BRIDGE_MODES,
  createDefaultMediaBridgeSettings,
} from "@/api/admin/settings";
import type {
  MediaBridgeSettings,
  MediaBridgeStorageSettings,
  MediaBridgeTenantOverride,
} from "@/api/admin/settings";
import TotpStepUpDialog from "@/components/auth/TotpStepUpDialog.vue";
import Icon from "@/components/icons/Icon.vue";
import {
  isStepUpBlocked,
  isStepUpCancelled,
  stepUpBlockReason,
  useStepUp,
} from "@/composables/useStepUp";
import { useAppStore } from "@/stores";
import { extractApiErrorMessage } from "@/utils/apiError";

const SectionTitle = defineComponent({
  props: {
    title: { type: String, required: true },
    description: { type: String, required: true },
  },
  setup(props) {
    return () => h("div", [
      h("h3", { class: "text-base font-semibold text-gray-900 dark:text-white" }, props.title),
      h("p", { class: "mt-1 text-sm text-gray-500 dark:text-gray-400" }, props.description),
    ]);
  },
});

const { t } = useI18n();
const appStore = useAppStore();
const mediaBridgeStepUp = useStepUp();
const loading = ref(true);
const loadFailed = ref(false);
const saving = ref(false);
const testingStorage = ref(false);
const form = reactive<MediaBridgeSettings>(createDefaultMediaBridgeSettings());
const storageBaseline = ref("");
const scopeModelsInput = ref("");
const scopeAccountIDsInput = ref("");
const allowedMimeTypesInput = ref("");
const supportedIngressProtocols = new Set<string>(MEDIA_BRIDGE_INGRESS_PROTOCOLS);

type CapacityNumericKey = Exclude<keyof MediaBridgeSettings["capacity"], "tenant_overrides">;
type ProtectionNumericKey = keyof MediaBridgeSettings["protection"];
type FilePolicyNumericKey = Exclude<keyof MediaBridgeSettings["file_policy"], "allowed_mime_types" | "deduplicate_within_request">;
type RetentionNumericKey = keyof MediaBridgeSettings["retention"];

const capacityFields: Array<{ key: CapacityNumericKey; label: string; hint: string; bytes?: boolean; rate?: boolean; max?: number }> = [
  { key: "max_inflight_requests", label: "admin.settings.mediaBridge.capacity.maxInflightRequests", hint: "admin.settings.mediaBridge.capacity.maxInflightRequestsHint" },
  { key: "max_inflight_decoded_bytes", label: "admin.settings.mediaBridge.capacity.maxInflightBytes", hint: "", bytes: true },
  { key: "max_bandwidth_bytes_per_second", label: "admin.settings.mediaBridge.capacity.maxBandwidth", hint: "", bytes: true, rate: true },
  { key: "burst_bytes", label: "admin.settings.mediaBridge.capacity.burstBytes", hint: "", bytes: true },
  { key: "admission_wait_ms", label: "admin.settings.mediaBridge.capacity.admissionWaitMs", hint: "admin.settings.mediaBridge.capacity.admissionWaitMsHint", max: 60000 },
  { key: "default_tenant_weight", label: "admin.settings.mediaBridge.capacity.defaultTenantWeight", hint: "admin.settings.mediaBridge.capacity.defaultTenantWeightHint" },
];

const protectionFields: Array<{ key: ProtectionNumericKey; label: string; max?: number; bytes?: boolean }> = [
  { key: "memory_soft_limit_percent", label: "admin.settings.mediaBridge.protection.memorySoftPercent", max: 100 },
  { key: "memory_hard_limit_percent", label: "admin.settings.mediaBridge.protection.memoryHardPercent", max: 100 },
  { key: "min_free_memory_bytes", label: "admin.settings.mediaBridge.protection.minFreeMemory", bytes: true },
  { key: "r2_error_rate_threshold_percent", label: "admin.settings.mediaBridge.protection.r2ErrorRate", max: 100 },
  { key: "r2_latency_threshold_ms", label: "admin.settings.mediaBridge.protection.r2LatencyMs", max: 300000 },
  { key: "r2_window_seconds", label: "admin.settings.mediaBridge.protection.r2WindowSeconds", max: 86400 },
  { key: "r2_open_seconds", label: "admin.settings.mediaBridge.protection.r2OpenSeconds", max: 86400 },
  { key: "r2_half_open_probes", label: "admin.settings.mediaBridge.protection.r2HalfOpenProbes" },
  { key: "r2_minimum_samples", label: "admin.settings.mediaBridge.protection.r2MinimumSamples" },
  { key: "r2_upload_timeout_seconds", label: "admin.settings.mediaBridge.protection.r2UploadTimeoutSeconds", max: 3600 },
];

const filePolicyFields: Array<{ key: FilePolicyNumericKey; label: string; hint: string; bytes?: boolean }> = [
  { key: "max_files_per_request", label: "admin.settings.mediaBridge.filePolicy.maxFiles", hint: "admin.settings.mediaBridge.filePolicy.maxFilesHint" },
  { key: "max_single_decoded_bytes", label: "admin.settings.mediaBridge.filePolicy.maxSingleBytes", hint: "", bytes: true },
  { key: "max_request_decoded_bytes", label: "admin.settings.mediaBridge.filePolicy.maxRequestBytes", hint: "", bytes: true },
];

const retentionFields: Array<{ key: RetentionNumericKey; label: string; hint: string; max?: number }> = [
  { key: "signed_url_ttl_seconds", label: "admin.settings.mediaBridge.retention.signedUrlTTL", hint: "admin.settings.mediaBridge.retention.signedUrlTTLHint", max: 604800 },
  { key: "request_end_delete_delay_seconds", label: "admin.settings.mediaBridge.retention.requestEndDeleteDelay", hint: "admin.settings.mediaBridge.retention.requestEndDeleteDelayHint", max: 604800 },
];

function uniqueStrings(input: string): string[] {
  return Array.from(new Set(input.split(/[\s,]+/).map((value) => value.trim()).filter(Boolean)));
}

function numberTokens(input: string): string[] {
  return input.split(/[\s,]+/).map((value) => value.trim()).filter(Boolean);
}

function parsePositiveIntegerList(input: string): number[] {
  return Array.from(new Set(numberTokens(input).map(Number).filter((value) => Number.isSafeInteger(value) && value > 0)));
}

function isSafeStorageEndpoint(value: string): boolean {
  if (!value) return true;
  try {
    const parsed = new URL(value);
    return parsed.protocol === "https:"
      && Boolean(parsed.host)
      && !parsed.username
      && !parsed.password
      && !parsed.search
      && !parsed.hash
      && parsed.pathname === "/";
  } catch {
    return false;
  }
}

function isValidObjectPrefix(value: string): boolean {
  const normalized = value.trim().replace(/^\/+|\/+$/g, "");
  return !normalized || (normalized.length <= 256
    && !normalized.includes("\\")
    && normalized.split("/").every((part) => part !== "" && part !== "." && part !== ".."));
}

function isValidStorageBucket(value: string): boolean {
  return Boolean(value)
    && value !== "."
    && value !== ".."
    && value.length <= 255
    && !/[/\\?#\s]/.test(value);
}

function hasUnsafeStorageControl(value: string): boolean {
  return value.includes("\r") || value.includes("\n") || value.includes(String.fromCharCode(0));
}

function storageWriteInput(storage: MediaBridgeStorageSettings): MediaBridgeStorageSettings {
  return {
    provider: storage.provider.trim(),
    endpoint: storage.endpoint.trim(),
    region: storage.region.trim(),
    bucket: storage.bucket.trim(),
    object_prefix: storage.object_prefix.trim(),
    access_key_id: storage.access_key_id.trim(),
    secret_access_key: storage.secret_access_key ?? "",
    secret_configured: storage.secret_configured,
    ready: storage.ready,
    force_path_style: storage.force_path_style,
  };
}

function storagePublicFingerprint(storage: MediaBridgeStorageSettings): string {
  const normalized = storageWriteInput(storage);
  return JSON.stringify({
    provider: normalized.provider,
    endpoint: normalized.endpoint,
    region: normalized.region,
    bucket: normalized.bucket,
    object_prefix: normalized.object_prefix,
    access_key_id: normalized.access_key_id,
    force_path_style: normalized.force_path_style,
  });
}

const storageHasChanges = computed(() =>
  storagePublicFingerprint(form.storage) !== storageBaseline.value
  || Boolean(form.storage.secret_access_key),
);
const storageDisplayReady = computed(() => form.storage.ready && !storageHasChanges.value);

const storageValidationError = computed(() => {
  if (form.storage.provider !== "r2") {
    return t("admin.settings.mediaBridge.validation.storageProvider");
  }
  if (!isSafeStorageEndpoint(form.storage.endpoint.trim())) {
    return t("admin.settings.mediaBridge.validation.storageEndpoint");
  }
  if (!isValidStorageBucket(form.storage.bucket.trim())) {
    return t("admin.settings.mediaBridge.validation.storageBucket");
  }
  if (!isValidObjectPrefix(form.storage.object_prefix)) {
    return t("admin.settings.mediaBridge.validation.objectPrefix");
  }
  if (
    !form.storage.endpoint.trim()
    || !form.storage.access_key_id.trim()
    || (!form.storage.secret_configured && !form.storage.secret_access_key)
  ) {
    return t("admin.settings.mediaBridge.validation.storageCredentialsRequired");
  }
  if (
    form.storage.region.length > 128
    || hasUnsafeStorageControl(form.storage.region)
    || form.storage.access_key_id.length > 1024
    || hasUnsafeStorageControl(form.storage.access_key_id)
    || (form.storage.secret_access_key?.length ?? 0) > 4096
    || hasUnsafeStorageControl(form.storage.secret_access_key ?? "")
  ) {
    return t("admin.settings.mediaBridge.validation.storageFields");
  }
  return "";
});

function applyStorageSettings(storage: MediaBridgeStorageSettings): void {
  Object.assign(form.storage, storage, { secret_access_key: "" });
  storageBaseline.value = storagePublicFingerprint(form.storage);
}

function applySettings(settings: MediaBridgeSettings): void {
  const defaults = createDefaultMediaBridgeSettings();
  const next: MediaBridgeSettings = {
    ...defaults,
    ...settings,
    scope: {
      ...defaults.scope,
      ...settings.scope,
      ingress_protocols: [...(settings.scope?.ingress_protocols ?? defaults.scope.ingress_protocols)],
      upstream_protocols: [MEDIA_BRIDGE_K3_VIDEO_EGRESS_PROTOCOL],
    },
    capacity: {
      ...defaults.capacity,
      ...settings.capacity,
      tenant_overrides: (settings.capacity?.tenant_overrides ?? []).map((item) => ({ ...item })),
    },
    protection: { ...defaults.protection, ...settings.protection },
    file_policy: { ...defaults.file_policy, ...settings.file_policy },
    retention: { ...defaults.retention, ...settings.retention },
    storage: {
      ...defaults.storage,
      ...settings.storage,
      secret_access_key: "",
    },
  };
  Object.assign(form, next);
  scopeModelsInput.value = next.scope.models.join(", ");
  scopeAccountIDsInput.value = next.scope.account_ids.join(", ");
  allowedMimeTypesInput.value = next.file_policy.allowed_mime_types.join(", ");
  storageBaseline.value = storagePublicFingerprint(next.storage);
}

function isNonNegativeInteger(value: number): boolean {
  return Number.isSafeInteger(value) && value >= 0;
}

const validationError = computed(() => {
  if (form.mode === "canary" && (!Number.isFinite(form.canary_percent) || form.canary_percent <= 0 || form.canary_percent > 100)) {
    return t("admin.settings.mediaBridge.validation.canaryPercent");
  }
  if (!Number.isFinite(form.canary_percent) || form.canary_percent < 0 || form.canary_percent > 100) {
    return t("admin.settings.mediaBridge.validation.percent");
  }
  if (numberTokens(scopeAccountIDsInput.value).some((value) => !Number.isSafeInteger(Number(value)) || Number(value) <= 0)) {
    return t("admin.settings.mediaBridge.validation.accountIds");
  }
  if (form.scope.ingress_protocols.some((protocol) => !supportedIngressProtocols.has(protocol))) {
    return t("admin.settings.mediaBridge.validation.ingressProtocols");
  }
  const capacityValues = capacityFields.map((field) => form.capacity[field.key]);
  const protectionValues = protectionFields.map((field) => form.protection[field.key]);
  const fileValues = filePolicyFields.map((field) => form.file_policy[field.key]);
  const retentionValues = retentionFields.map((field) => form.retention[field.key]);
  if (![...capacityValues, ...protectionValues, ...fileValues, ...retentionValues].every(isNonNegativeInteger)) {
    return t("admin.settings.mediaBridge.validation.nonNegativeInteger");
  }
  if (
    form.capacity.admission_wait_ms > 60000
    || form.protection.r2_latency_threshold_ms > 300000
    || form.protection.r2_window_seconds > 86400
    || form.protection.r2_open_seconds > 86400
    || form.protection.r2_upload_timeout_seconds < 1
    || form.protection.r2_upload_timeout_seconds > 3600
    || form.retention.signed_url_ttl_seconds > 604800
    || form.retention.request_end_delete_delay_seconds > 604800
  ) {
    return t("admin.settings.mediaBridge.validation.safetyBounds");
  }
  if (form.capacity.default_tenant_weight < 1) {
    return t("admin.settings.mediaBridge.validation.tenantWeight");
  }
  if (form.protection.memory_soft_limit_percent > 100 || form.protection.memory_hard_limit_percent > 100 || form.protection.r2_error_rate_threshold_percent > 100) {
    return t("admin.settings.mediaBridge.validation.percent");
  }
  if (
    form.protection.memory_soft_limit_percent > 0
    && form.protection.memory_hard_limit_percent > 0
    && form.protection.memory_soft_limit_percent >= form.protection.memory_hard_limit_percent
  ) {
    return t("admin.settings.mediaBridge.validation.memoryLimits");
  }
  const tenantIDs = new Set<number>();
  for (const item of form.capacity.tenant_overrides) {
    if (!Number.isSafeInteger(item.tenant_id) || item.tenant_id <= 0 || !Number.isSafeInteger(item.weight) || item.weight <= 0) {
      return t("admin.settings.mediaBridge.validation.tenantOverride");
    }
    if (tenantIDs.has(item.tenant_id)) {
      return t("admin.settings.mediaBridge.validation.duplicateTenant");
    }
    tenantIDs.add(item.tenant_id);
    if (![item.max_inflight_requests, item.max_inflight_decoded_bytes, item.max_bandwidth_bytes_per_second].every(isNonNegativeInteger)) {
      return t("admin.settings.mediaBridge.validation.nonNegativeInteger");
    }
  }
  const allowedMIMETypes = uniqueStrings(allowedMimeTypesInput.value).map((value) => value.toLowerCase());
  if (allowedMIMETypes.length !== 1 || allowedMIMETypes[0] !== "video/mp4") {
    return t("admin.settings.mediaBridge.validation.mimeTypes");
  }
  if (form.retention.signed_url_ttl_seconds <= 0) {
    return t("admin.settings.mediaBridge.validation.retentionTTL");
  }
  if (storageHasChanges.value && storageValidationError.value) {
    return storageValidationError.value;
  }
  if (form.mode === "on" || form.mode === "canary") {
    if (storageValidationError.value) return storageValidationError.value;
    if (!form.storage.ready && !storageHasChanges.value) {
      return t("admin.settings.mediaBridge.validation.storageNotReady");
    }
  }
  return "";
});

function formatBytes(value: number, rate = false): string {
  if (!value) return t("admin.settings.mediaBridge.zeroValue");
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let amount = value;
  let index = 0;
  while (amount >= 1024 && index < units.length - 1) {
    amount /= 1024;
    index += 1;
  }
  const formatted = `${amount >= 10 || Number.isInteger(amount) ? amount.toFixed(0) : amount.toFixed(1)} ${units[index]}`;
  return rate ? `${formatted}/s` : formatted;
}

function addTenantOverride(): void {
  const used = new Set(form.capacity.tenant_overrides.map((item) => item.tenant_id));
  let tenantID = 1;
  while (used.has(tenantID)) tenantID += 1;
  const item: MediaBridgeTenantOverride = {
    tenant_id: tenantID,
    weight: 1,
    max_inflight_requests: 0,
    max_inflight_decoded_bytes: 0,
    max_bandwidth_bytes_per_second: 0,
  };
  form.capacity.tenant_overrides.push(item);
}

function removeTenantOverride(index: number): void {
  form.capacity.tenant_overrides.splice(index, 1);
}

function buildPayload(): MediaBridgeSettings {
  return {
    version: form.version,
    mode: form.mode,
    canary_percent: form.canary_percent,
    scope: {
      ingress_protocols: [...form.scope.ingress_protocols],
      upstream_protocols: [MEDIA_BRIDGE_K3_VIDEO_EGRESS_PROTOCOL],
      models: uniqueStrings(scopeModelsInput.value),
      account_ids: parsePositiveIntegerList(scopeAccountIDsInput.value),
    },
    capacity: {
      ...form.capacity,
      tenant_overrides: form.capacity.tenant_overrides.map((item) => ({ ...item })),
    },
    protection: { ...form.protection },
    file_policy: {
      ...form.file_policy,
      allowed_mime_types: uniqueStrings(allowedMimeTypesInput.value).map((value) => value.toLowerCase()),
    },
    retention: { ...form.retention },
    storage: {
      provider: form.storage.provider.trim(),
      endpoint: form.storage.endpoint.trim(),
      region: form.storage.region.trim(),
      bucket: form.storage.bucket.trim(),
      object_prefix: form.storage.object_prefix.trim(),
      access_key_id: form.storage.access_key_id.trim(),
      secret_access_key: form.storage.secret_access_key ?? "",
      secret_configured: form.storage.secret_configured,
      ready: form.storage.ready,
      force_path_style: form.storage.force_path_style,
    },
  };
}

async function loadSettings(): Promise<void> {
  loading.value = true;
  loadFailed.value = false;
  try {
    applySettings(await adminAPI.settings.getMediaBridgeSettings());
  } catch (error: unknown) {
    loadFailed.value = true;
    appStore.showError(extractApiErrorMessage(error, t("admin.settings.mediaBridge.loadFailed")));
  } finally {
    loading.value = false;
  }
}

async function saveSettings(): Promise<void> {
  if (validationError.value) return;
  saving.value = true;
  let savedStorage: MediaBridgeStorageSettings | null = null;
  try {
    const payload = buildPayload();
    const shouldSaveStorage = storageHasChanges.value;
    const updated = await mediaBridgeStepUp.run(async () => {
      if (shouldSaveStorage) {
        savedStorage = await adminAPI.settings.updateMediaBridgeStorage(payload.storage);
      }
      return adminAPI.settings.updateMediaBridgeSettings(payload);
    });
    applySettings({
      ...updated,
      storage: updated.storage ?? savedStorage ?? payload.storage,
    });
    appStore.showSuccess(t("admin.settings.mediaBridge.saved"));
  } catch (error: unknown) {
    if (savedStorage) applyStorageSettings(savedStorage);
    if (isStepUpCancelled(error)) return;
    if (reportStepUpBlocked(error)) return;
    appStore.showError(extractApiErrorMessage(error, t("admin.settings.mediaBridge.saveFailed")));
  } finally {
    saving.value = false;
  }
}

function reportStepUpBlocked(error: unknown): boolean {
  if (!isStepUpBlocked(error)) return false;
  appStore.showError(
    stepUpBlockReason(error) === "STEP_UP_ADMIN_API_KEY_FORBIDDEN"
      ? t("stepUp.adminApiKeyForbidden")
      : t("stepUp.notEnabled"),
  );
  return true;
}

async function testStorage(): Promise<void> {
  if (storageValidationError.value) return;
  testingStorage.value = true;
  try {
    await mediaBridgeStepUp.run(() =>
      adminAPI.settings.testMediaBridgeStorage(storageWriteInput(form.storage)),
    );
    appStore.showSuccess(t("admin.settings.mediaBridge.storage.testSucceeded"));
  } catch (error: unknown) {
    if (isStepUpCancelled(error)) return;
    if (reportStepUpBlocked(error)) return;
    appStore.showError(extractApiErrorMessage(error, t("admin.settings.mediaBridge.storage.testFailed")));
  } finally {
    testingStorage.value = false;
  }
}

onMounted(loadSettings);
</script>

<style scoped>
.field-label {
  @apply mb-2 block text-sm font-medium text-gray-700 dark:text-gray-300;
}

.field-hint {
  @apply mt-1.5 text-xs text-gray-500 dark:text-gray-400;
}
</style>
