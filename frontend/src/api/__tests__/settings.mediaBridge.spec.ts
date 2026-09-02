import { beforeEach, describe, expect, it, vi } from "vitest";

const { get, put, post } = vi.hoisted(() => ({
  get: vi.fn(),
  put: vi.fn(),
  post: vi.fn(),
}));

vi.mock("@/api/client", () => ({
  apiClient: { get, put, post },
}));

import {
  MEDIA_BRIDGE_INGRESS_PROTOCOLS,
  MEDIA_BRIDGE_K3_VIDEO_EGRESS_PROTOCOL,
  createDefaultMediaBridgeSettings,
  getMediaBridgeSettings,
  testMediaBridgeStorage,
  updateMediaBridgeSettings,
  updateMediaBridgeStorage,
} from "@/api/admin/settings";

describe("admin media bridge runtime settings API", () => {
  beforeEach(() => {
    get.mockReset();
    put.mockReset();
    post.mockReset();
  });

  it("keeps policy and storage on separate endpoints", async () => {
    const settings = createDefaultMediaBridgeSettings();
    settings.mode = "canary";
    settings.canary_percent = 5;
    settings.scope.ingress_protocols = ["openai_responses"];
    settings.scope.upstream_protocols = ["openai_responses"];
    settings.storage.access_key_id = "access-id";
    settings.storage.secret_access_key = "write-only-secret";
    const publicSettings = structuredClone(settings);
    publicSettings.storage.secret_access_key = "must-not-be-retained";
    publicSettings.storage.secret_configured = true;
    publicSettings.storage.ready = true;
    get.mockResolvedValueOnce({ data: publicSettings });
    put
      .mockResolvedValueOnce({ data: publicSettings })
      .mockResolvedValueOnce({ data: publicSettings.storage });
    post.mockResolvedValueOnce({ data: { ok: true } });

    await expect(getMediaBridgeSettings()).resolves.toMatchObject({
      storage: { secret_access_key: "", secret_configured: true, ready: true },
    });
    await expect(updateMediaBridgeSettings(settings)).resolves.toMatchObject({
      storage: { secret_access_key: "" },
    });
    await expect(updateMediaBridgeStorage(settings.storage)).resolves.toMatchObject({
      secret_access_key: "",
    });
    await expect(testMediaBridgeStorage(settings.storage)).resolves.toEqual({ ok: true });

    expect(get).toHaveBeenCalledWith("/admin/settings/media-bridge");
    expect(put.mock.calls[0]).toEqual([
      "/admin/settings/media-bridge",
      expect.objectContaining({
        scope: expect.objectContaining({
          ingress_protocols: ["openai_responses"],
          upstream_protocols: [MEDIA_BRIDGE_K3_VIDEO_EGRESS_PROTOCOL],
        }),
      }),
    ]);
    expect(put.mock.calls[0][1]).not.toHaveProperty("storage");
    const storagePayload = {
      provider: "r2",
      endpoint: "",
      region: "auto",
      bucket: "",
      object_prefix: "media-bridge/",
      access_key_id: "access-id",
      secret_access_key: "write-only-secret",
      force_path_style: true,
    };
    expect(put.mock.calls[1]).toEqual([
      "/admin/settings/media-bridge/storage",
      storagePayload,
    ]);
    expect(post).toHaveBeenCalledWith(
      "/admin/settings/media-bridge/storage/test",
      storagePayload,
    );
  });

  it("defaults to disabled storage with a blank write-only secret", () => {
    const settings = createDefaultMediaBridgeSettings();

    expect(MEDIA_BRIDGE_INGRESS_PROTOCOLS).toEqual([
      "openai_chat_completions",
      "openai_responses",
    ]);
    expect(MEDIA_BRIDGE_K3_VIDEO_EGRESS_PROTOCOL).toBe("openai_chat_completions");
    expect(settings.mode).toBe("off");
    expect(settings.scope.ingress_protocols).toEqual(MEDIA_BRIDGE_INGRESS_PROTOCOLS);
    expect(settings.scope.upstream_protocols).toEqual([MEDIA_BRIDGE_K3_VIDEO_EGRESS_PROTOCOL]);
    expect(settings.scope.models).toEqual(["kimi-k3"]);
    expect(settings.capacity.max_inflight_requests).toBe(0);
    expect(settings.capacity.admission_wait_ms).toBe(200);
    expect(settings.capacity.default_tenant_weight).toBe(10);
    expect(settings.retention).toEqual({
      signed_url_ttl_seconds: 3600,
      request_end_delete_delay_seconds: 900,
    });
    expect(settings.storage).toEqual({
      provider: "r2",
      endpoint: "",
      region: "auto",
      bucket: "",
      object_prefix: "media-bridge/",
      access_key_id: "",
      secret_access_key: "",
      secret_configured: false,
      ready: false,
      force_path_style: true,
    });
  });
});
