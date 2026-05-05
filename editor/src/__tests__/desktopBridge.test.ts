import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// Vitest runs in node mode (see vite.config.ts test.environment). The
// desktopBridge module reads `typeof window !== "undefined"` and walks
// `window.go.main.App.*`, so we install a stub `window` on globalThis
// before importing the module.
//
// Importing dynamically inside the tests means each describe block can
// reset the stub, get a fresh module instance, and verify the
// browser-mode rejection vs desktop-mode dispatch.

const win = globalThis as unknown as { window?: { go?: unknown; runtime?: unknown } };

afterEach(() => {
  delete win.window;
});

describe("desktopBridge — browser mode (no window.go)", () => {
  beforeEach(() => {
    win.window = {};
  });

  it("isDesktop() returns false", async () => {
    const mod = await import("@/lib/desktopBridge");
    expect(mod.isDesktop()).toBe(false);
  });

  it("getServerURL rejects with a stable error", async () => {
    const mod = await import("@/lib/desktopBridge");
    await expect(mod.desktop.getServerURL()).rejects.toThrow("Not available in browser mode");
  });

  it("listProjects rejects with a stable error", async () => {
    const mod = await import("@/lib/desktopBridge");
    await expect(mod.desktop.listProjects()).rejects.toThrow("Not available in browser mode");
  });
});

describe("desktopBridge — desktop mode (mocked bindings)", () => {
  beforeEach(() => {
    win.window = {
      go: {
        main: {
          App: {
            GetServerURL: vi.fn().mockResolvedValue("http://127.0.0.1:54321/"),
            GetSessionToken: vi.fn().mockResolvedValue("tok"),
            GetAppInfo: vi.fn().mockResolvedValue({
              version: "v0.1.0",
              commit: "abc123",
              os: "linux",
              arch: "amd64",
              license: "MIT",
              homepage: "",
              issue_tracker: "",
              documentation: "",
            }),
            ListProjects: vi.fn().mockResolvedValue([]),
            GetCurrentProject: vi.fn().mockResolvedValue(null),
            IsFirstRunPending: vi.fn().mockResolvedValue(true),
            MarkFirstRunDone: vi.fn().mockResolvedValue(undefined),
            DetectExternalCLIs: vi.fn().mockResolvedValue([
              { name: "claude", found: false, install_url: "https://x" },
            ]),
            GetSecretStatuses: vi.fn().mockResolvedValue([]),
            SetSecret: vi.fn().mockResolvedValue(undefined),
            DeleteSecret: vi.fn().mockResolvedValue(undefined),
            CheckForUpdate: vi.fn().mockResolvedValue(null),
            DownloadAndApplyUpdate: vi.fn().mockResolvedValue(undefined),
            OpenExternal: vi.fn().mockResolvedValue(undefined),
            RevealInFinder: vi.fn().mockResolvedValue(undefined),
            Quit: vi.fn().mockResolvedValue(undefined),
            AddProject: vi.fn().mockResolvedValue({ id: "p", name: "p", dir: "/tmp/p", last_opened: "" }),
            AddProjectSilently: vi.fn().mockResolvedValue({ id: "p", name: "p", dir: "/tmp/p", last_opened: "" }),
            RemoveProject: vi.fn().mockResolvedValue(undefined),
            SwitchProject: vi.fn().mockResolvedValue(undefined),
            PickProjectDirectory: vi.fn().mockResolvedValue("/tmp/p"),
            ScaffoldProject: vi.fn().mockResolvedValue(undefined),
            GetKnownSecretKeys: vi.fn().mockResolvedValue(["ANTHROPIC_API_KEY"]),
          },
        },
      },
    };
  });

  it("isDesktop() returns true", async () => {
    const mod = await import("@/lib/desktopBridge");
    expect(mod.isDesktop()).toBe(true);
  });

  it("getServerURL forwards to the binding", async () => {
    const mod = await import("@/lib/desktopBridge");
    const url = await mod.desktop.getServerURL();
    expect(url).toBe("http://127.0.0.1:54321/");
  });

  it("isFirstRunPending forwards to the binding", async () => {
    const mod = await import("@/lib/desktopBridge");
    const pending = await mod.desktop.isFirstRunPending();
    expect(pending).toBe(true);
  });

  it("setSecret forwards key + value", async () => {
    const mod = await import("@/lib/desktopBridge");
    await mod.desktop.setSecret("ANTHROPIC_API_KEY", "sk-test");
    const fn = (win.window as { go: { main: { App: { SetSecret: ReturnType<typeof vi.fn> } } } }).go.main.App.SetSecret;
    expect(fn).toHaveBeenCalledWith("ANTHROPIC_API_KEY", "sk-test");
  });

  it("addProjectSilently forwards to the non-emitting onboarding binding", async () => {
    const mod = await import("@/lib/desktopBridge");
    await mod.desktop.addProjectSilently("/tmp/p");
    const fn = (win.window as { go: { main: { App: { AddProjectSilently: ReturnType<typeof vi.fn> } } } }).go.main.App.AddProjectSilently;
    expect(fn).toHaveBeenCalledWith("/tmp/p");
  });

  it("getDesktopWsBase returns absolute ws URL with token query", async () => {
    const mod = await import("@/lib/desktopBridge");
    mod.resetDesktopWsCache();
    const url = await mod.getDesktopWsBase("/api/ws");
    // The bindings stub returns http://127.0.0.1:54321/ and token "tok".
    // The dialer must produce ws://127.0.0.1:54321/api/ws?t=tok so the
    // local server's session middleware accepts the upgrade across the
    // wails:// → http://127.0.0.1 origin boundary.
    expect(url).toBe("ws://127.0.0.1:54321/api/ws?t=tok");
  });

  it("getDesktopWsBase reuses cache when serverURL unchanged across calls", async () => {
    const mod = await import("@/lib/desktopBridge");
    mod.resetDesktopWsCache();
    const url1 = await mod.getDesktopWsBase("/api/ws");
    const url2 = await mod.getDesktopWsBase("/api/ws/runs/abc");
    // Cache hit on the second call: same serverURL, different path.
    expect(url1).toBe("ws://127.0.0.1:54321/api/ws?t=tok");
    expect(url2).toBe("ws://127.0.0.1:54321/api/ws/runs/abc?t=tok");
    const fn = (win.window as { go: { main: { App: { GetServerURL: ReturnType<typeof vi.fn> } } } }).go.main.App.GetServerURL;
    // GetServerURL was called for each path resolution (we re-fetch token
    // + URL every call so a project switch on a fresh page load takes
    // effect immediately). The cache is a string-rebuild fast path, not a
    // binding-call cache. So both calls hit the binding once each.
    expect(fn).toHaveBeenCalledTimes(2);
  });

  it("getDesktopWsBase rebuilds when serverURL changes (project switch)", async () => {
    const mod = await import("@/lib/desktopBridge");
    mod.resetDesktopWsCache();
    const url1 = await mod.getDesktopWsBase("/api/ws");
    expect(url1).toBe("ws://127.0.0.1:54321/api/ws?t=tok");
    // Simulate a project switch: the embedded server rebinds on a fresh
    // ephemeral port. The cached wsBase from the previous serverURL must
    // be invalidated so the dialer doesn't hand back a dead URL.
    const App = (win.window as { go: { main: { App: { GetServerURL: ReturnType<typeof vi.fn> } } } }).go.main.App;
    App.GetServerURL.mockResolvedValueOnce("http://127.0.0.1:60000/");
    const url2 = await mod.getDesktopWsBase("/api/ws");
    expect(url2).toBe("ws://127.0.0.1:60000/api/ws?t=tok");
  });
});

describe("desktopBridge — getDesktopWsBase in browser mode", () => {
  beforeEach(() => {
    win.window = {};
  });

  it("returns null in browser mode so caller can fall back to relative URL", async () => {
    const mod = await import("@/lib/desktopBridge");
    mod.resetDesktopWsCache();
    const url = await mod.getDesktopWsBase("/api/ws");
    expect(url).toBeNull();
  });
});
