// @vitest-environment jsdom

import { act, renderHook, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { useSessionModelPref } from "./useSessionModelPref";

const api = vi.hoisted(() => ({
  fetchModelPref: vi.fn(),
  saveModelPref: vi.fn(),
  clearModelPref: vi.fn(),
}));

vi.mock("@/api/modelPrefs", () => api);

describe("useSessionModelPref", () => {
  beforeEach(() => {
    vi.resetAllMocks();
  });

  it("drops the previous key's choice even when the new key cannot load", async () => {
    let rejectCopilot!: (error: Error) => void;
    api.fetchModelPref
      .mockResolvedValueOnce({
        key: "whats-next",
        model: "openai/gpt-5.6-sol",
        backend: "claw",
        effort: "high",
        set: true,
      })
      .mockImplementationOnce(
        () =>
          new Promise((_resolve, reject) => {
            rejectCopilot = reject;
          }),
      );

    const { result, rerender } = renderHook(
      ({ prefKey }) => useSessionModelPref(prefKey),
      { initialProps: { prefKey: "whats-next" as string | null } },
    );

    await waitFor(() => {
      expect(result.current.current()).toEqual({
        model: "openai/gpt-5.6-sol",
        backend: "claw",
        effort: "high",
      });
    });

    rerender({ prefKey: "copilot" });

    expect(result.current.choice).toEqual({});
    expect(result.current.current()).toEqual({});
    expect(result.current.set).toBe(false);
    expect(result.current.loading).toBe(true);

    act(() => rejectCopilot(new Error("preferences unavailable")));
    await waitFor(() => {
      expect(result.current.loading).toBe(false);
      expect(result.current.available).toBe(false);
    });
    expect(result.current.current()).toEqual({});
  });
});
