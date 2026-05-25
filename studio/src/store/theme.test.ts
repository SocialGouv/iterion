// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";

function stubThemeStorage(opts: {
  getItem?: () => string | null;
  setItem?: (key: string, value: string) => void;
}) {
  Object.defineProperty(window, "localStorage", {
    configurable: true,
    value: {
      getItem: opts.getItem ?? (() => null),
      setItem: opts.setItem ?? (() => {}),
    },
  });
}

afterEach(() => {
  vi.resetModules();
});

describe("theme store persistence", () => {
  it("initializes when localStorage and matchMedia are unavailable", async () => {
    stubThemeStorage({
      getItem: () => {
        throw new Error("storage blocked");
      },
    });
    Object.defineProperty(window, "matchMedia", {
      configurable: true,
      value: undefined,
    });

    const { initializeTheme, useThemeStore } = await import("./theme");

    expect(() => initializeTheme()).not.toThrow();
    expect(useThemeStore.getState().mode).toBe("system");
    expect(useThemeStore.getState().resolved).toBe("dark");
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
  });

  it("applies theme changes even when persistence fails", async () => {
    stubThemeStorage({
      setItem: () => {
        throw new Error("quota exceeded");
      },
    });

    const { useThemeStore } = await import("./theme");

    expect(() => useThemeStore.getState().setMode("light")).not.toThrow();
    expect(useThemeStore.getState().mode).toBe("light");
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
  });
});
