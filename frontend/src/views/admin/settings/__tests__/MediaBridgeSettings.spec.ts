import { beforeEach, describe, expect, it, vi } from "vitest";
import { flushPromises, mount } from "@vue/test-utils";

const {
  getSettings,
  updateSettings,
  updateStorage,
  testStorage,
  stepUpRun,
  showError,
  showSuccess,
} = vi.hoisted(() => ({
  getSettings: vi.fn(),
  updateSettings: vi.fn(),
  updateStorage: vi.fn(),
  testStorage: vi.fn(),
  stepUpRun: vi.fn(async (action: () => Promise<unknown>) => action()),
  showError: vi.fn(),
  showSuccess: vi.fn(),
}));

vi.mock("@/api", () => ({
  adminAPI: {
    settings: {
      getMediaBridgeSettings: getSettings,
      updateMediaBridgeSettings: updateSettings,
      updateMediaBridgeStorage: updateStorage,
      testMediaBridgeStorage: testStorage,
    },
  },
}));

vi.mock("@/composables/useStepUp", () => ({
  useStepUp: () => ({ run: stepUpRun }),
  isStepUpBlocked: () => false,
  isStepUpCancelled: () => false,
  stepUpBlockReason: () => "",
}));

vi.mock("@/components/auth/TotpStepUpDialog.vue", () => ({
  default: { template: '<div data-testid="step-up-dialog" />' },
}));

vi.mock("@/stores", () => ({
  useAppStore: () => ({ showError, showSuccess }),
}));

vi.mock("@/utils/apiError", () => ({
  extractApiErrorMessage: (_error: unknown, fallback: string) => fallback,
}));

vi.mock("vue-i18n", async (importOriginal) => {
  const actual = await importOriginal<typeof import("vue-i18n")>();
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params ? `${key}:${JSON.stringify(params)}` : key,
    }),
  };
});

import { createDefaultMediaBridgeSettings } from "@/api/admin/settings";
import MediaBridgeSettings from "../MediaBridgeSettings.vue";

describe("MediaBridgeSettings", () => {
  beforeEach(() => {
    getSettings.mockReset();
    updateSettings.mockReset();
    updateStorage.mockReset();
    testStorage.mockReset();
    stepUpRun.mockClear();
    showError.mockReset();
    showSuccess.mockReset();
    const settings = createDefaultMediaBridgeSettings();
    settings.version = 1;
    settings.capacity.max_inflight_decoded_bytes = 8 * 1024 * 1024 * 1024;
    settings.storage.endpoint = "https://example.r2.cloudflarestorage.com";
    settings.storage.bucket = "worldcodes-sub2api-media-bridge";
    settings.storage.access_key_id = "access-id";
    settings.storage.secret_configured = true;
    settings.storage.ready = true;
    getSettings.mockResolvedValue(structuredClone(settings));
    updateSettings.mockImplementation(async (payload) => structuredClone(payload));
    updateStorage.mockImplementation(async (storage) => ({
      ...structuredClone(storage),
      secret_access_key: "",
      secret_configured: true,
      ready: true,
    }));
    testStorage.mockImplementation(async (storage) => ({
      ...structuredClone(storage),
      secret_access_key: "",
      secret_configured: true,
      ready: true,
    }));
  });

  it("saves changed storage first and protects the save with step-up", async () => {
    const wrapper = mount(MediaBridgeSettings);
    await flushPromises();

    expect(wrapper.get('[data-testid="media-bridge-mode"]').element).toHaveProperty("value", "off");
    const ingressProtocols = wrapper.get('[data-testid="media-bridge-ingress-protocols"]');
    expect(ingressProtocols.text()).toContain("ingress.openai_chat_completions");
    expect(ingressProtocols.text()).toContain("ingress.openai_responses");
    const chatIngress = wrapper.get('[data-testid="media-bridge-ingress-openai_chat_completions"]');
    expect((chatIngress.element as HTMLInputElement).checked).toBe(true);
    expect((wrapper.get('[data-testid="media-bridge-ingress-openai_responses"]').element as HTMLInputElement).checked).toBe(true);
    expect(wrapper.get('[data-testid="media-bridge-final-egress"]').text()).toContain("finalEgressChat");
    expect(wrapper.text()).toContain("ingressHint");
    expect(wrapper.text()).toContain("vendorExtensionNotice");
    expect(wrapper.text()).toContain("hotApplyTitle");
    expect(wrapper.text()).toContain("8 GiB");

    await wrapper.get('[data-testid="media-bridge-mode"]').setValue("canary");
    await wrapper.get('[data-testid="media-bridge-canary-percent"]').setValue("12");
    await chatIngress.setValue(false);
    await wrapper.get('[data-testid="media-bridge-scope-models"]').setValue("kimi-k3, kimi-k3-video");
    await wrapper.get('[data-testid="media-bridge-add-tenant"]').trigger("click");
    await wrapper.get('[data-testid="media-bridge-object-prefix"]').setValue("media-bridge");
    await wrapper.get('[data-testid="media-bridge-save"]').trigger("click");
    await flushPromises();

    expect(stepUpRun).toHaveBeenCalledTimes(1);
    expect(updateStorage).toHaveBeenCalledTimes(1);
    expect(updateStorage.mock.calls[0][0]).toMatchObject({
      endpoint: "https://example.r2.cloudflarestorage.com",
      bucket: "worldcodes-sub2api-media-bridge",
      object_prefix: "media-bridge",
      access_key_id: "access-id",
      secret_access_key: "",
      force_path_style: true,
    });
    expect(updateSettings).toHaveBeenCalledTimes(1);
    const payload = updateSettings.mock.calls[0][0];
    expect(payload).toMatchObject({
      version: 1,
      mode: "canary",
      canary_percent: 12,
      scope: {
        ingress_protocols: ["openai_responses"],
        upstream_protocols: ["openai_chat_completions"],
        models: ["kimi-k3", "kimi-k3-video"],
      },
      storage: {
        provider: "r2",
        endpoint: "https://example.r2.cloudflarestorage.com",
        bucket: "worldcodes-sub2api-media-bridge",
        object_prefix: "media-bridge",
        access_key_id: "access-id",
      },
    });
    expect(payload.capacity.tenant_overrides).toEqual([
      {
        tenant_id: 1,
        weight: 1,
        max_inflight_requests: 0,
        max_inflight_decoded_bytes: 0,
        max_bandwidth_bytes_per_second: 0,
      },
    ]);
    expect(showSuccess).toHaveBeenCalled();
  });

  it("saves an empty ingress scope as unrestricted and normalizes legacy egress", async () => {
    const settings = createDefaultMediaBridgeSettings();
    settings.scope.ingress_protocols = ["openai_responses"];
    settings.scope.upstream_protocols = ["openai_responses"];
    settings.storage.endpoint = "https://example.r2.cloudflarestorage.com";
    settings.storage.bucket = "worldcodes-sub2api-media-bridge";
    settings.storage.access_key_id = "access-id";
    settings.storage.secret_configured = true;
    settings.storage.ready = true;
    getSettings.mockResolvedValueOnce(settings);

    const wrapper = mount(MediaBridgeSettings);
    await flushPromises();
    const responsesIngress = wrapper.get('[data-testid="media-bridge-ingress-openai_responses"]');
    expect((responsesIngress.element as HTMLInputElement).checked).toBe(true);
    expect((wrapper.get('[data-testid="media-bridge-ingress-openai_chat_completions"]').element as HTMLInputElement).checked).toBe(false);
    await responsesIngress.setValue(false);
    await wrapper.get('[data-testid="media-bridge-save"]').trigger("click");
    await flushPromises();

    expect(updateSettings).toHaveBeenCalledTimes(1);
    expect(updateSettings.mock.calls[0][0].scope.ingress_protocols).toEqual([]);
    expect(updateSettings.mock.calls[0][0].scope.upstream_protocols).toEqual([
      "openai_chat_completions",
    ]);
  });

  it("does not retain a returned secret and blank secret preserves unchanged storage", async () => {
    const settings = createDefaultMediaBridgeSettings();
    settings.storage.endpoint = "https://example.r2.cloudflarestorage.com";
    settings.storage.bucket = "worldcodes-sub2api-media-bridge";
    settings.storage.access_key_id = "access-id";
    settings.storage.secret_access_key = "must-not-be-held";
    settings.storage.secret_configured = true;
    settings.storage.ready = true;
    getSettings.mockResolvedValueOnce(settings);

    const wrapper = mount(MediaBridgeSettings);
    await flushPromises();

    expect(wrapper.get('[data-testid="media-bridge-secret-access-key"]').element).toHaveProperty("value", "");
    await wrapper.get('[data-testid="media-bridge-save"]').trigger("click");
    await flushPromises();

    expect(updateStorage).not.toHaveBeenCalled();
    expect(updateSettings).toHaveBeenCalledTimes(1);
  });

  it("runs the real storage probe through step-up without clearing an unsaved secret", async () => {
    const settings = createDefaultMediaBridgeSettings();
    settings.storage.endpoint = "https://example.r2.cloudflarestorage.com";
    settings.storage.bucket = "worldcodes-sub2api-media-bridge";
    settings.storage.access_key_id = "access-id";
    getSettings.mockResolvedValueOnce(settings);
    const wrapper = mount(MediaBridgeSettings);
    await flushPromises();

    const secret = wrapper.get('[data-testid="media-bridge-secret-access-key"]');
    await secret.setValue("new-secret");
    await wrapper.get('[data-testid="media-bridge-test-storage"]').trigger("click");
    await flushPromises();

    expect(stepUpRun).toHaveBeenCalledTimes(1);
    expect(testStorage).toHaveBeenCalledWith(expect.objectContaining({
      access_key_id: "access-id",
      secret_access_key: "new-secret",
    }));
    expect(secret.element).toHaveProperty("value", "new-secret");
    expect(wrapper.get('[data-testid="media-bridge-storage-status"]').text()).toContain("notReady");
  });

  it("requires a non-zero percentage in canary mode", async () => {
    const wrapper = mount(MediaBridgeSettings);
    await flushPromises();

    await wrapper.get('[data-testid="media-bridge-mode"]').setValue("canary");

    expect(wrapper.get('[data-testid="media-bridge-validation-error"]').text()).toContain("canaryPercent");
    expect(wrapper.get('[data-testid="media-bridge-save"]').attributes("disabled")).toBeDefined();
  });
});
