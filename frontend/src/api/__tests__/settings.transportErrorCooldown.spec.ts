import { beforeEach, describe, expect, it, vi } from "vitest";

const { get, put } = vi.hoisted(() => ({
  get: vi.fn(),
  put: vi.fn(),
}));

vi.mock("@/api/client", () => ({ apiClient: { get, put } }));

import {
  getOpenAITransportErrorCooldownSettings,
  updateOpenAITransportErrorCooldownSettings,
} from "@/api/admin/settings";

describe("OpenAI transport error cooldown settings API", () => {
  beforeEach(() => {
    get.mockReset();
    put.mockReset();
  });

  it("loads and hot-updates the dedicated admin runtime setting", async () => {
    const settings = { enabled: false, cooldown_seconds: 30 };
    get.mockResolvedValueOnce({ data: settings });
    put.mockResolvedValueOnce({ data: settings });

    await expect(getOpenAITransportErrorCooldownSettings()).resolves.toEqual(
      settings,
    );
    await expect(
      updateOpenAITransportErrorCooldownSettings(settings),
    ).resolves.toEqual(settings);

    expect(get).toHaveBeenCalledWith(
      "/admin/settings/openai-transport-error-cooldown",
    );
    expect(put).toHaveBeenCalledWith(
      "/admin/settings/openai-transport-error-cooldown",
      settings,
    );
  });
});
