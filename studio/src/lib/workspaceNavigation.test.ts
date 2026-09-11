// @vitest-environment jsdom

import { describe, expect, it, vi } from "vitest";

import {
  requestBrowserWorkspaceSwitch,
  WORKSPACE_SWITCH_REQUEST,
  WORKSPACE_SWITCH_RESULT,
} from "./workspaceNavigation";

function harness() {
  const listeners = new Set<(event: MessageEvent) => void>();
  const parent = { postMessage: vi.fn() };
  const currentWindow = {
    parent,
    location: { origin: "http://iterion.test" },
    addEventListener: (_type: "message", listener: (event: MessageEvent) => void) => {
      listeners.add(listener);
    },
    removeEventListener: (_type: "message", listener: (event: MessageEvent) => void) => {
      listeners.delete(listener);
    },
    setTimeout: window.setTimeout.bind(window),
    clearTimeout: window.clearTimeout.bind(window),
  };
  const dispatch = (data: unknown, origin = currentWindow.location.origin) => {
    for (const listener of listeners) {
      listener({ data, origin, source: parent } as unknown as MessageEvent);
    }
  };
  return { currentWindow, parent, dispatch, listeners };
}

describe("requestBrowserWorkspaceSwitch", () => {
  it("correlates the parent acknowledgement before resolving", async () => {
    const h = harness();
    const pending = requestBrowserWorkspaceSwitch("project-b", {
      currentWindow: h.currentWindow,
      scoped: true,
      wailsHosted: false,
      requestId: "request-1",
    });

    expect(h.parent.postMessage).toHaveBeenCalledWith(
      {
        source: "iterion-pane",
        type: WORKSPACE_SWITCH_REQUEST,
        projectId: "project-b",
        requestId: "request-1",
      },
      "http://iterion.test",
    );

    h.dispatch({
      source: "iterion-shell",
      type: WORKSPACE_SWITCH_RESULT,
      requestId: "another-request",
      ok: true,
    });
    h.dispatch({
      source: "iterion-shell",
      type: WORKSPACE_SWITCH_RESULT,
      requestId: "request-1",
      ok: true,
    });

    await expect(pending).resolves.toBeUndefined();
    expect(h.listeners.size).toBe(0);
  });

  it("rejects a correlated shell error", async () => {
    const h = harness();
    const pending = requestBrowserWorkspaceSwitch("project-b", {
      currentWindow: h.currentWindow,
      scoped: true,
      wailsHosted: false,
      requestId: "request-2",
    });
    h.dispatch({
      source: "iterion-shell",
      type: WORKSPACE_SWITCH_RESULT,
      requestId: "request-2",
      ok: false,
      error: "Project runtime is unavailable",
    });

    await expect(pending).rejects.toThrow("Project runtime is unavailable");
  });

  it("settles without messaging outside a browser workspace pane", async () => {
    const h = harness();
    await expect(
      requestBrowserWorkspaceSwitch("project-b", {
        currentWindow: h.currentWindow,
        scoped: false,
        wailsHosted: false,
      }),
    ).resolves.toBeUndefined();
    expect(h.parent.postMessage).not.toHaveBeenCalled();
  });

  it("times out when the shell never acknowledges", async () => {
    vi.useFakeTimers();
    const h = harness();
    const pending = requestBrowserWorkspaceSwitch("project-b", {
      currentWindow: h.currentWindow,
      scoped: true,
      wailsHosted: false,
      requestId: "request-3",
      timeoutMs: 25,
    });
    const assertion = expect(pending).rejects.toThrow(
      "The workspace did not confirm the project switch",
    );
    await vi.advanceTimersByTimeAsync(25);
    await assertion;
    expect(h.listeners.size).toBe(0);
    vi.useRealTimers();
  });
});
