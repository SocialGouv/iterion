// @vitest-environment jsdom
// S1: a redial carries the birth the page learned after the first dial. The
// first socket of a view opened before any birth is known dials without an
// expectation; its snapshot teaches the run's birth; a transport drop then
// redials, and the URL of that second socket — derived again at the dial,
// with the real URL builder — names the learned birth, so the server
// refuses it if the run was replaced meanwhile instead of attaching this
// view's cursors to the successor.
import { act, cleanup, renderHook } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { EXPECTED_BIRTH_PARAM, forgetLostRunsForTest, knownRunBirth } from "@/lib/lostRuns";
import { createRunStore, RunStoreProvider } from "@/store/run";

import { useRunWebSocket } from "./useRunWebSocket";

const A = "BIRTHAAAAAAAAAAAAAAAAAAAAA";

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
  // A frame from the server, as the browser surfaces it.
  serverSend(env: unknown): void {
    this.onmessage?.(new MessageEvent("message", { data: JSON.stringify(env) }));
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

function expectationOf(url: string): string | null {
  return new URL(url).searchParams.get(EXPECTED_BIRTH_PARAM);
}

function socketAt(i: number): FakeWebSocket {
  const ws = FakeWebSocket.instances[i];
  if (!ws) throw new Error(`no socket #${i + 1}: ${FakeWebSocket.instances.length} dialed`);
  return ws;
}

beforeEach(() => {
  vi.useFakeTimers();
  // The birth and lost-run registries are page-scoped: each test is a fresh page.
  forgetLostRunsForTest();
  FakeWebSocket.instances.length = 0;
  vi.stubGlobal("WebSocket", FakeWebSocket);
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("the run socket's redial", () => {
  it("carries the birth the first socket's snapshot taught", async () => {
    const store = createRunStore();
    const wrapper = ({ children }: { children: ReactNode }) => createElement(RunStoreProvider, { store, children });
    renderHook(() => useRunWebSocket("run-a"), { wrapper });
    await flushMicrotasks();
    expect(FakeWebSocket.instances).toHaveLength(1);
    const first = socketAt(0);
    expect(first.url).toContain("/ws/runs/run-a");
    expect(expectationOf(first.url)).toBeNull();

    act(() => first.open());
    act(() =>
      first.serverSend({
        type: "snapshot",
        payload: { run: { id: "run-a", workflow_name: "wf", status: "running", birth_id: A }, executions: [], last_seq: 0 },
      }),
    );
    expect(knownRunBirth("", "run-a")).toBe(A);

    act(() => first.serverClose(1006, ""));
    expect(store.getState().wsState).toBe("reconnecting");
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1_500);
    });
    await flushMicrotasks();
    expect(FakeWebSocket.instances).toHaveLength(2);
    const second = socketAt(1);
    expect(second.url).toContain("/ws/runs/run-a");
    expect(expectationOf(second.url)).toBe(A);
  });
});
