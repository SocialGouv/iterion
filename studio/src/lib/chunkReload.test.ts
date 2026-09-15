// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  RELOAD_COOLDOWN_MS,
  installChunkReload,
  reloadForStaleChunk,
} from "./chunkReload";

const reload = vi.fn();

beforeEach(() => {
  window.sessionStorage.clear();
  reload.mockClear();
  // jsdom's location.reload is not writable; replace the whole accessor.
  Object.defineProperty(window, "location", {
    configurable: true,
    value: { ...window.location, reload },
  });
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("reloadForStaleChunk", () => {
  it("reloads on the first stale chunk", () => {
    expect(reloadForStaleChunk(1_000)).toBe(true);
    expect(reload).toHaveBeenCalledTimes(1);
  });

  it("refuses a second reload inside the cooldown", () => {
    reloadForStaleChunk(1_000);
    reload.mockClear();

    expect(reloadForStaleChunk(1_000 + RELOAD_COOLDOWN_MS - 1)).toBe(false);
    expect(reload).not.toHaveBeenCalled();
  });

  it("allows a fresh reload once the cooldown elapsed", () => {
    reloadForStaleChunk(1_000);
    reload.mockClear();

    expect(reloadForStaleChunk(1_000 + RELOAD_COOLDOWN_MS)).toBe(true);
    expect(reload).toHaveBeenCalledTimes(1);
  });
});

describe("per-document budget", () => {
  function at(pathname: string) {
    Object.defineProperty(window, "location", {
      configurable: true,
      value: { ...window.location, pathname, reload },
    });
  }

  it("does not let one document spend another's allowance", () => {
    // The shell and its panes are same-origin documents sharing ONE
    // sessionStorage. A single key would make the second caller a no-op.
    at("/");
    expect(reloadForStaleChunk(1_000)).toBe(true);
    at("/x/conn-a/");
    expect(reloadForStaleChunk(1_000)).toBe(true);
    at("/x/conn-b/");
    expect(reloadForStaleChunk(1_000)).toBe(true);
    expect(reload).toHaveBeenCalledTimes(3);
  });

  it("still holds the cooldown within one document", () => {
    at("/x/conn-a/");
    expect(reloadForStaleChunk(1_000)).toBe(true);
    expect(reloadForStaleChunk(1_500)).toBe(false);
  });
});

describe("storage refusal", () => {
  it("declines the recovery instead of throwing out of the listener", () => {
    const original = Object.getOwnPropertyDescriptor(window, "sessionStorage");
    Object.defineProperty(window, "sessionStorage", {
      configurable: true,
      get() {
        throw new DOMException("The operation is insecure.", "SecurityError");
      },
    });
    try {
      installChunkReload();
      const event = new CustomEvent("vite:preloadError", { cancelable: true });
      expect(() => window.dispatchEvent(event)).not.toThrow();
      expect(reload).not.toHaveBeenCalled();
      // Uncancelled on purpose: Vite rethrows, so the failure stays visible
      // rather than being swallowed by a recovery that could not run.
      expect(event.defaultPrevented).toBe(false);
    } finally {
      if (original) {
        Object.defineProperty(window, "sessionStorage", original);
      }
    }
  });
});

describe("installChunkReload", () => {
  it("reloads and cancels the event when it takes over the failure", () => {
    installChunkReload();

    const event = new CustomEvent("vite:preloadError", { cancelable: true });
    window.dispatchEvent(event);

    expect(reload).toHaveBeenCalledTimes(1);
    expect(event.defaultPrevented).toBe(true);
  });

  it("lets the error propagate once the allowance is spent", () => {
    installChunkReload();
    window.dispatchEvent(new CustomEvent("vite:preloadError", { cancelable: true }));
    reload.mockClear();

    // The witness for the other direction: a handler that always cancelled
    // would pass the first case while silently swallowing every later failure.
    const second = new CustomEvent("vite:preloadError", { cancelable: true });
    window.dispatchEvent(second);

    expect(reload).not.toHaveBeenCalled();
    expect(second.defaultPrevented).toBe(false);
  });
});
