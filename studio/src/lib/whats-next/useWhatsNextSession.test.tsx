// @vitest-environment jsdom

import { act, cleanup, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { FirstClassBot } from "./firstClassBots";
import { createRunStore, RunStoreProvider } from "@/store/run";
import { useWhatsNextSession } from "./useWhatsNextSession";

const discovery = vi.hoisted(() => ({
  onAttached: null as ((runId: string) => void) | null,
}));

const sessionScope = vi.hoisted(() => ({ ready: true }));

vi.mock("@/hooks/useRunWebSocket", () => ({
  useRunWebSocket: () => undefined,
}));

vi.mock("@/hooks/useSessionModelPref", () => ({
  useSessionModelPref: () => ({
    choice: {},
    set: false,
    loading: false,
    saving: false,
    error: null,
    available: true,
    save: vi.fn(),
    reset: vi.fn(),
    current: () => ({}),
  }),
}));

vi.mock("./sessionScope", () => ({
  useSessionScope: () => ({
    scopeKey: "project-a",
    ready: sessionScope.ready,
    repoScopeEnabled: false,
    overview: false,
    activeRepo: null,
    projectId: "project-a",
    launchRepo: null,
  }),
}));

vi.mock("./useSessionDiscovery", () => ({
  useSessionDiscovery: (opts: { onAttached: (runId: string) => void }) => {
    discovery.onAttached = opts.onAttached;
    return { discoveryError: null, retryDiscovery: vi.fn() };
  },
}));

vi.mock("./useSessionLifecycle", () => ({
  useSessionLifecycle: () => ({
    launch: vi.fn(),
    newSession: vi.fn(),
    lastVarsRef: { current: null },
  }),
}));

vi.mock("./useSessionMessages", () => ({
  useSessionMessages: () => [],
}));

vi.mock("./useSessionSteering", () => ({
  useSessionSteering: () => ({
    submitHumanAnswer: vi.fn(),
    resume: vi.fn(),
  }),
}));

const bot = (id: string): FirstClassBot => ({
  id,
  label: id,
  description: id,
  workflowPath: `bots/${id}/main.bot`,
  launcherVars: [],
  nodeMap: {},
});

afterEach(() => {
  cleanup();
  discovery.onAttached = null;
  sessionScope.ready = true;
});

describe("useWhatsNextSession", () => {
  it("drops the attached run and store when the bot identity changes", async () => {
    const store = createRunStore();
    const wrapper = ({ children }: { children: ReactNode }) => (
      <RunStoreProvider store={store}>{children}</RunStoreProvider>
    );
    const { result, rerender } = renderHook(
      ({ selectedBot }) => useWhatsNextSession(selectedBot),
      {
        initialProps: { selectedBot: bot("whats-next") },
        wrapper,
      },
    );

    act(() => discovery.onAttached?.("run-a"));
    await waitFor(() => {
      expect(result.current.runId).toBe("run-a");
      expect(store.getState().runId).toBe("run-a");
    });

    rerender({ selectedBot: bot("copilot") });

    await waitFor(() => {
      expect(result.current.runId).toBeNull();
      expect(store.getState().runId).toBeNull();
    });
  });

  it("drops the previous bot's run even when scope resolution is not ready", async () => {
    sessionScope.ready = false;
    const store = createRunStore();
    const wrapper = ({ children }: { children: ReactNode }) => (
      <RunStoreProvider store={store}>{children}</RunStoreProvider>
    );
    const { result, rerender } = renderHook(
      ({ selectedBot }) => useWhatsNextSession(selectedBot),
      {
        initialProps: { selectedBot: bot("whats-next") },
        wrapper,
      },
    );

    act(() => discovery.onAttached?.("run-a"));
    await waitFor(() => expect(result.current.runId).toBe("run-a"));

    rerender({ selectedBot: bot("copilot") });

    await waitFor(() => {
      expect(result.current.runId).toBeNull();
      expect(store.getState().runId).toBeNull();
    });
  });
});
