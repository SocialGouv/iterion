// @vitest-environment jsdom
// The run socket's reconnect policy at its close. Any close the server does
// not name scope_lost is a transport blip: the hook redials with backoff and
// resumes from the cursors it holds. A scope_lost close says the run the
// socket was opened on is gone (replaced or deleted under its ID — the
// origin-lost close and the older deletion close alike) or the caller's
// scope no longer holds: redialing would attach those cursors to whatever
// run holds the ID now, so the hook stops in wsState "lost" and nothing —
// backoff, watchdog, focus or network revalidation — dials again.
import { act, cleanup, renderHook } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { forgetLostRunsForTest } from "@/lib/lostRuns";
import { createRunStore, RunStoreProvider } from "@/store/run";

import { useRunWebSocket } from "./useRunWebSocket";

vi.mock("./runWs/url", () => ({
  deriveWsUrl: async (runId: string) => `ws://studio.test/api/ws/runs/${runId}`,
}));

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
  // The server's close frame, as the browser surfaces it.
  serverClose(code: number, reason: string): void {
    this.readyState = FakeWebSocket.CLOSED;
    this.onclose?.(new CloseEvent("close", { code, reason }));
  }
}

const flushMicrotasks = async (): Promise<void> => {
  await act(async () => {
    for (let i = 0; i < 5; i++) await Promise.resolve();
  });
};

async function openedSocket(store: ReturnType<typeof createRunStore>): Promise<FakeWebSocket> {
  const wrapper = ({ children }: { children: ReactNode }) =>
    createElement(RunStoreProvider, { store, children });
  renderHook(() => useRunWebSocket("run-a"), { wrapper });
  await flushMicrotasks();
  const [ws] = FakeWebSocket.instances;
  if (!ws || FakeWebSocket.instances.length !== 1) {
    throw new Error(`want one socket, got ${FakeWebSocket.instances.length}`);
  }
  act(() => ws.open());
  expect(store.getState().wsState).toBe("open");
  expect(JSON.parse(ws.sent[0] ?? "{}").type).toBe("subscribe");
  return ws;
}

beforeEach(() => {
  vi.useFakeTimers();
  // The lost-run registry is page-scoped: each test is a fresh page.
  forgetLostRunsForTest();
  FakeWebSocket.instances.length = 0;
  vi.stubGlobal("WebSocket", FakeWebSocket);
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("the run socket's close", () => {
  it.each([
    ["the origin-lost close", 1000, "scope_lost: the run this socket watches was replaced or deleted"],
    ["the deletion close", 1000, "scope_lost: the run this socket watches is gone"],
    ["the standing close", 1008, "scope_lost: your standing in the run's team no longer holds"],
  ])("stops on %s and never redials", async (_name, code, reason) => {
    const store = createRunStore();
    const ws = await openedSocket(store);
    act(() => ws.serverClose(code, reason));
    expect(store.getState().wsState).toBe("lost");
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5 * 60_000);
    });
    act(() => {
      window.dispatchEvent(new Event("online"));
      document.dispatchEvent(new Event("visibilitychange"));
    });
    await flushMicrotasks();
    expect(FakeWebSocket.instances).toHaveLength(1);
    expect(store.getState().wsState).toBe("lost");
  });

  // Resume, answer, merge and steering bump the reconnect token after their
  // HTTP call; one completing after the close must not redial the old
  // run's cursors either.
  it("does not redial when the reconnect token is requested after scope_lost", async () => {
    const store = createRunStore();
    const ws = await openedSocket(store);
    act(() => ws.serverClose(1000, "scope_lost: the run this socket watches was replaced or deleted"));
    const token = store.getState().wsReconnectToken;
    act(() => store.getState().requestWsReconnect());
    await flushMicrotasks();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    expect(FakeWebSocket.instances).toHaveLength(1);
    expect(store.getState().wsReconnectToken).toBe(token);
    expect(store.getState().wsState).toBe("lost");
  });

  it.each([
    ["a transport drop", 1006, ""],
    ["the lifetime cap", 1000, "lifetime_reached: reopen the socket to present a credential again"],
  ])("redials after %s", async (_name, code, reason) => {
    const store = createRunStore();
    const ws = await openedSocket(store);
    act(() => ws.serverClose(code, reason));
    expect(store.getState().wsState).toBe("reconnecting");
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_500);
    });
    await flushMicrotasks();
    expect(FakeWebSocket.instances).toHaveLength(2);
  });
});
