// @vitest-environment jsdom

import { act, renderHook, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { ReactNode } from "react";

import { createRunStore, RunStoreProvider } from "@/store/run";

import type { FirstClassBot } from "./firstClassBots";
import { useSessionDiscovery } from "./useSessionDiscovery";

const discovery = vi.hoisted(() => ({
  attachSessionRun: vi.fn(),
  findLiveRunForBot: vi.fn(),
}));

vi.mock("./startupDiscovery", () => ({
  attachSessionRun: (...args: unknown[]) => discovery.attachSessionRun(...args),
  findLiveRunForBot: (...args: unknown[]) => discovery.findLiveRunForBot(...args),
}));

vi.mock("./sessionStorage", () => ({
  recallSessionRunId: () => null,
  forgetSessionRunId: vi.fn(),
}));

const bot: FirstClassBot = {
  id: "copilot",
  label: "Copi",
  description: "Assistant",
  workflowPath: "bots/copilot/main.bot",
  launcherVars: [],
  nodeMap: {},
};

function wrapper({ children }: { children: ReactNode }) {
  return <RunStoreProvider store={createRunStore()}>{children}</RunStoreProvider>;
}

const baseOpts = {
  bot,
  scopeKey: "project-a",
  repoScopeEnabled: false,
  overview: false,
  activeRepo: null,
};

beforeEach(() => {
  vi.resetAllMocks();
});

describe("useSessionDiscovery attachRunId teardown", () => {
  it("aborts an in-flight attach when the effect is torn down", async () => {
    let captured: {
      signal: AbortSignal;
      isCancelled: () => boolean;
    } | null = null;
    discovery.attachSessionRun.mockImplementation(
      (opts: { signal: AbortSignal; isCancelled: () => boolean }) => {
        captured = opts;
        return new Promise(() => {});
      },
    );
    const { unmount } = renderHook(
      () =>
        useSessionDiscovery({
          ...baseOpts,
          onAttached: vi.fn(),
          setStatus: vi.fn(),
          discover: false,
          attachRunId: "run-1",
        }),
      { wrapper },
    );
    await waitFor(() => expect(captured).not.toBeNull());
    expect(captured!.signal.aborted).toBe(false);
    expect(captured!.isCancelled()).toBe(false);
    act(() => {
      unmount();
    });
    expect(captured!.signal.aborted).toBe(true);
    expect(captured!.isCancelled()).toBe(true);
  });

  it("unlatches the gate so a later attachRunId can attach", async () => {
    const attached: string[] = [];
    discovery.attachSessionRun.mockImplementation(
      async (opts: { runId: string; isCancelled: () => boolean }) => {
        if (opts.isCancelled()) return false;
        attached.push(opts.runId);
        return true;
      },
    );
    const { rerender } = renderHook(
      (props: { attachRunId: string | null }) =>
        useSessionDiscovery({
          ...baseOpts,
          onAttached: vi.fn(),
          setStatus: vi.fn(),
          discover: false,
          attachRunId: props.attachRunId,
        }),
      { wrapper, initialProps: { attachRunId: "run-1" as string | null } },
    );
    await waitFor(() => expect(attached).toEqual(["run-1"]));

    rerender({ attachRunId: "run-2" });
    await waitFor(() => expect(attached).toEqual(["run-1", "run-2"]));
  });

  it("unlatches the !discover path so a later attachRunId can attach", async () => {
    const attached: string[] = [];
    discovery.attachSessionRun.mockImplementation(
      async (opts: { runId: string; isCancelled: () => boolean }) => {
        if (opts.isCancelled()) return false;
        attached.push(opts.runId);
        return true;
      },
    );
    const { rerender } = renderHook(
      (props: { attachRunId: string | null; discover: boolean }) =>
        useSessionDiscovery({
          ...baseOpts,
          onAttached: vi.fn(),
          setStatus: vi.fn(),
          discover: props.discover,
          attachRunId: props.attachRunId,
        }),
      {
        wrapper,
        initialProps: { attachRunId: null as string | null, discover: false },
      },
    );

    rerender({ attachRunId: "run-later", discover: false });
    await waitFor(() => expect(attached).toEqual(["run-later"]));
  });
});
