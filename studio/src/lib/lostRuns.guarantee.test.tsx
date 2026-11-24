// @vitest-environment jsdom
// The page's guarantee once a run socket closed scope_lost for run X: nothing
// on the page addresses X again — no HTTP request, no socket — whatever
// spells the call: an action (merge, answer, resume, tags, delete after a
// confirmation opened earlier, export, rename, cancel, pause, steering), a
// poller (children, assistant watches), the reconnect token, an online
// event, a URL lookup resolving after the loss, a view on X unmounted and
// mounted again, the shell and CDP sockets. Global fetch and WebSocket are
// the witnesses. The same calls for another run go out, so a silent
// transport cannot pass this test.
import { act, cleanup, render, renderHook } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createElement, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  apiURL,
  answerInteraction,
  cancelRun,
  commitAndFinalizeRun,
  deleteRun,
  fetchArtifactFile,
  fetchAttachment,
  fetchToolBlob,
  getMergeConflicts,
  getRun,
  getRunChildren,
  getRunTags,
  getRunWorkflow,
  listAssistantRunWatches,
  loadEvents,
  mergeRun,
  pauseRun,
  renameRun,
  resumeRun,
  setRunTags,
} from "@/api/runs";
import { queueMessage } from "@/api/queueMessages";
import { bumpLoop } from "@/api/runSteering";
import { RunShellPanel } from "@/components/Runs/RunShellPanel";
import AssistantWatchBadge from "@/components/Runs/runHeader/AssistantWatchBadge";
import { useRunSnapshot } from "@/components/Runs/runView/useRunSnapshot";
import { useRunChildren } from "@/hooks/useRunChildren";
import { useRunWebSocket } from "@/hooks/useRunWebSocket";
import { CDPClient } from "@/lib/cdpClient";
import { forgetLostRunsForTest, isRunLost, LostRunError, runFetch } from "@/lib/lostRuns";
import { createRunStore, RunStoreProvider } from "@/store/run";

// The run socket's URL lookup, deferrable: a test can hold lookups pending
// and resolve them in any order.
const lookups: { runId: string; resolve: (url: string) => void }[] = [];
// Every lookup, by run: a lookup can mint a pane ticket, so a lost run gets
// none.
const lookedUp: string[] = [];
let deferLookups = false;
vi.mock("@/hooks/runWs/url", () => ({
  deriveWsUrl: (runId: string) => {
    lookedUp.push(runId);
    const url = `ws://studio.test/api/ws/runs/${encodeURIComponent(runId)}`;
    if (!deferLookups) return Promise.resolve(url);
    return new Promise<string>((resolve) => lookups.push({ runId, resolve: () => resolve(url) }));
  },
}));

vi.mock("@xterm/xterm", () => ({
  Terminal: class {
    cols = 80;
    rows = 24;
    write = vi.fn();
    loadAddon = vi.fn();
    open = vi.fn();
    dispose = vi.fn();
    onData = vi.fn();
  },
}));
vi.mock("@xterm/addon-fit", () => ({ FitAddon: class { fit = vi.fn(); } }));
vi.mock("@xterm/xterm/css/xterm.css", () => ({}));

class FakeWebSocket {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSING = 2;
  static CLOSED = 3;
  static instances: FakeWebSocket[] = [];
  readyState = FakeWebSocket.CONNECTING;
  binaryType = "blob";
  onopen: ((ev: Event) => void) | null = null;
  onclose: ((ev: CloseEvent) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  sent: string[] = [];
  constructor(public url: string) {
    FakeWebSocket.instances.push(this);
  }
  open(): void {
    this.readyState = FakeWebSocket.OPEN;
    this.onopen?.(new Event("open"));
  }
  send(data: string): void {
    this.sent.push(data);
  }
  close(): void {
    this.readyState = FakeWebSocket.CLOSED;
  }
  serverClose(code: number, reason: string): void {
    this.readyState = FakeWebSocket.CLOSED;
    this.onclose?.(new CloseEvent("close", { code, reason }));
  }
}

const requested: string[] = [];
const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
  requested.push(String(input));
  return new Response("{}", { status: 200, headers: { "Content-Type": "application/json" } });
});

const SCOPE_LOST = "scope_lost: the run this socket watches was replaced or deleted";

function addresses(url: string, runId: string): boolean {
  return url.includes(`/runs/${runId}`);
}
function requestsTo(runId: string): string[] {
  return requested.filter((u) => addresses(u, runId));
}
function socketsTo(runId: string): FakeWebSocket[] {
  return FakeWebSocket.instances.filter((ws) => addresses(ws.url, runId));
}

async function flush(): Promise<void> {
  await act(async () => {
    for (let i = 0; i < 8; i++) await Promise.resolve();
  });
}

function withStore(store: ReturnType<typeof createRunStore>, client?: QueryClient) {
  return ({ children }: { children: ReactNode }) => {
    const inner = createElement(RunStoreProvider, { store, children });
    return client ? createElement(QueryClientProvider, { client }, inner) : inner;
  };
}

// loseRun opens the run socket for runId and lets the server close it
// scope_lost: the one event that marks the run lost for the page.
async function loseRun(runId: string) {
  const store = createRunStore();
  const view = renderHook(() => useRunWebSocket(runId), { wrapper: withStore(store) });
  await flush();
  const ws = socketsTo(runId).pop();
  if (!ws) throw new Error(`no socket opened for ${runId}`);
  act(() => ws.open());
  act(() => ws.serverClose(1000, SCOPE_LOST));
  return { store, view };
}

// Every run-addressed action and read a run view can issue. Each either
// leaves (another run) or is refused before leaving (a lost run).
async function driveActions(runId: string): Promise<PromiseSettledResult<unknown>[]> {
  const calls: Promise<unknown>[] = [
    mergeRun(runId, { merge_strategy: "squash" }),
    commitAndFinalizeRun(runId, { message: "m" } as never),
    getMergeConflicts(runId),
    answerInteraction(runId, "i-1", "yes"),
    resumeRun(runId, { answers: { q: "a" } }),
    setRunTags(runId, ["t"]),
    getRunTags(runId),
    deleteRun(runId),
    getRun(runId),
    loadEvents(runId),
    renameRun(runId, "n"),
    cancelRun(runId),
    pauseRun(runId),
    getRunChildren(runId),
    getRunWorkflow(runId),
    listAssistantRunWatches(runId),
    queueMessage(runId, "hello"),
    bumpLoop(runId, "loop", 1),
    fetchToolBlob(runId, "tool-1", "output"),
    fetchArtifactFile(runId, "out.txt"),
    fetchAttachment(runId, "a.png"),
    runFetch(`/api/runs/${encodeURIComponent(runId)}/browser/attach`, { method: "POST" }),
    runFetch(apiURL(`/runs/${encodeURIComponent(runId)}/log`)),
  ];
  return Promise.allSettled(calls);
}

beforeEach(() => {
  vi.useFakeTimers();
  forgetLostRunsForTest();
  requested.length = 0;
  lookups.length = 0;
  lookedUp.length = 0;
  deferLookups = false;
  FakeWebSocket.instances.length = 0;
  fetchMock.mockClear();
  vi.stubGlobal("fetch", fetchMock);
  vi.stubGlobal("WebSocket", FakeWebSocket);
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe = vi.fn();
      disconnect = vi.fn();
    },
  );
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("after scope_lost, the page never addresses the lost run again", () => {
  it("refuses every action, poller, reconnect and socket for the lost run, and serves another run", async () => {
    const { store, view } = await loseRun("run-x");
    expect(isRunLost("", "run-x")).toBe(true);
    const socketsBefore = socketsTo("run-x").length;
    const lookupsBefore = lookedUp.filter((id) => id === "run-x").length;
    requested.length = 0;

    const refused = await driveActions("run-x");
    for (const r of refused) {
      expect(r.status).toBe("rejected");
      expect((r as PromiseRejectedResult).reason).toBeInstanceOf(LostRunError);
    }

    // The reconnect token, an online event and the visibility probe.
    act(() => {
      store.getState().requestWsReconnect();
      window.dispatchEvent(new Event("online"));
      document.dispatchEvent(new Event("visibilitychange"));
    });
    await flush();

    // Pollers: the children poll and the assistant-watch badge, on a
    // QueryClient of their own, across many poll periods.
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    renderHook(
      () => useRunChildren("run-x", true, { refetchIntervalMs: 3_000, parentActive: true }),
      { wrapper: withStore(createRunStore(), client) },
    );
    render(createElement(AssistantWatchBadge, { runId: "run-x" }), {
      wrapper: withStore(createRunStore(), client),
    });

    // A view on X unmounted, then mounted again on a fresh store: it starts
    // lost, dials nothing and its snapshot fetch never leaves.
    view.unmount();
    expect(store.getState().wsState).toBe("lost");
    const fresh = createRunStore();
    renderHook(() => useRunWebSocket("run-x"), { wrapper: withStore(fresh) });
    renderHook(() => useRunSnapshot("run-x"), { wrapper: withStore(fresh) });
    await flush();
    expect(fresh.getState().wsState).toBe("lost");

    // The shell drawer and the CDP client: run sockets of their own.
    const shell = render(
      createElement(RunShellPanel, { open: true, onOpenChange: () => undefined, runId: "drawer-mount" }),
    );
    shell.rerender(createElement(RunShellPanel, { open: true, onOpenChange: () => undefined, runId: "run-x" }));
    let cdpRefusal: unknown = null;
    void new CDPClient({ runId: "run-x", sessionId: "s" }).connect().catch((err: unknown) => {
      cdpRefusal = err;
    });
    await flush();
    expect(cdpRefusal).toBeInstanceOf(LostRunError);

    await act(async () => {
      await vi.advanceTimersByTimeAsync(120_000);
    });
    await flush();

    expect(requestsTo("run-x")).toEqual([]);
    expect(socketsTo("run-x")).toHaveLength(socketsBefore);
    expect(lookedUp.filter((id) => id === "run-x")).toHaveLength(lookupsBefore);

    // Positive control: the same calls for another run leave.
    requested.length = 0;
    const served = await driveActions("run-y");
    expect(served.filter((r) => r.status === "rejected" && r.reason instanceof LostRunError)).toEqual([]);
    expect(requestsTo("run-y").length).toBeGreaterThanOrEqual(served.length);
    renderHook(() => useRunWebSocket("run-y"), { wrapper: withStore(createRunStore()) });
    void new CDPClient({ runId: "run-y", sessionId: "s" }).connect().catch(() => undefined);
    await flush();
    expect(socketsTo("run-y").length).toBeGreaterThanOrEqual(2);
  });

  // A URL lookup still pending when the run is lost resolves into nothing:
  // the check stands after the await, right before the open.
  it("does not dial when a URL lookup resolves after the loss", async () => {
    deferLookups = true;
    const store = createRunStore();
    renderHook(() => useRunWebSocket("run-late"), { wrapper: withStore(store) });
    await flush();
    // The first lookup is pending; a network-back event starts a second.
    act(() => window.dispatchEvent(new Event("online")));
    await flush();
    expect(lookups.length).toBe(2);
    const [first, second] = lookups;
    if (!first || !second) throw new Error("two lookups expected");
    act(() => first.resolve(""));
    await flush();
    const [ws] = socketsTo("run-late");
    if (!ws) throw new Error("the first lookup opened no socket");
    act(() => ws.open());
    act(() => ws.serverClose(1000, SCOPE_LOST));
    act(() => second.resolve(""));
    await flush();
    expect(socketsTo("run-late")).toHaveLength(1);
    expect(store.getState().wsState).toBe("lost");
  });
});
