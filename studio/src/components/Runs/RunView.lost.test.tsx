// @vitest-environment jsdom
//
// S2 (RunView): a run lost before any snapshot landed shows the origin-lost
// banner instead of the loading skeleton. The page's lost-run registry
// already holds the run (a socket of another view closed scope_lost, a
// mutation was refused run_origin_lost): no request by its ID will be
// answered, so a skeleton would pulse forever. The run socket hook — the
// real one — starts the view lost without dialing; the REST snapshot hook is
// stubbed to "still loading" so the skeleton is the only alternative.
import { act, cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { forgetLostRunsForTest, markRunLost } from "@/lib/lostRuns";
import { createRunStore, RunStoreProvider } from "@/store/run";

vi.mock("./runView/useRunSnapshot", () => ({
  useRunSnapshot: () => ({ loadFailed: null, handleRetryLoad: vi.fn(), refreshSnapshot: vi.fn() }),
}));
// A vendor icon barrel on the console's import path resolves its ESM
// subpaths in a way vitest cannot follow (@lobehub/icons via ProviderIcon
// and BackendBadge); pure presentation, never rendered before a snapshot.
vi.mock("@/components/icons/ProviderIcon", () => ({
  ProviderIcon: () => null,
}));
vi.mock("@/components/icons/BackendBadge", () => ({
  BackendBadge: () => null,
  effectiveBackend: () => "claw",
}));

import RunView from "./RunView";

const TITLE = "Ce run a été remplacé ou supprimé, ou votre accès a changé.";

class FakeWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSING = 2;
  static CLOSED = 3;
  static instances: FakeWebSocket[] = [];
  readyState = FakeWebSocket.CONNECTING;
  onopen: ((ev: Event) => void) | null = null;
  onclose: ((ev: CloseEvent) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  constructor(public url: string) {
    FakeWebSocket.instances.push(this);
  }
  send(): void {}
  close(): void {
    this.readyState = FakeWebSocket.CLOSED;
  }
}

async function flush(): Promise<void> {
  await act(async () => {
    for (let i = 0; i < 8; i++) await Promise.resolve();
  });
}

function mount(): void {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <RunStoreProvider store={createRunStore()}>
        <RunView runId="r" />
      </RunStoreProvider>
    </QueryClientProvider>,
  );
}

function lostBanner(): HTMLElement | null {
  return screen.queryAllByRole("alert").find((el) => el.textContent?.includes(TITLE)) ?? null;
}

beforeEach(() => {
  forgetLostRunsForTest();
  FakeWebSocket.instances.length = 0;
  vi.stubGlobal("WebSocket", FakeWebSocket);
  // Whatever RunView fetches beside the stubbed snapshot (the workflow IR)
  // stays pending: nothing this test reads depends on it.
  vi.stubGlobal("fetch", vi.fn(() => new Promise<Response>(() => {})));
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("RunView before any snapshot", () => {
  it("shows the origin-lost banner, not the skeleton, for a lost run", async () => {
    markRunLost("", "r");
    mount();
    await flush();
    const alert = lostBanner();
    expect(alert, "the origin-lost banner").not.toBeNull();
    expect(alert?.closest("[lang]")?.getAttribute("lang")).toBe("fr");
    expect(screen.getByRole("button", { name: "Recharger" })).toBeTruthy();
    expect(screen.queryByText("Loading run console…")).toBeNull();
    // Lost from the start: the view never dials the run.
    expect(FakeWebSocket.instances.filter((s) => s.url.includes("/ws/runs/r"))).toHaveLength(0);
  });

  it("shows the skeleton, and no banner, while a live run loads", async () => {
    mount();
    await flush();
    expect(screen.getByText("Loading run console…")).toBeTruthy();
    expect(lostBanner()).toBeNull();
  });
});
