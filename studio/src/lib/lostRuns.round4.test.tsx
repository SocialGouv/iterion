// @vitest-environment jsdom
// Round 4 of the expected birth: rows pin a view without ever losing a run
// (R4-C1), a snapshot's birth is keyed by the store it was read from (R4-C4),
// a malformed expectation
// close is not redialed (R4-A5), and the children poll and the memoized media
// URLs wait for / rebuild on a learned birth (R4-C3). Global fetch and
// WebSocket are the witnesses; the server is the stub's answers.
import { act, cleanup, fireEvent, render, renderHook, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createElement, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { SESSION_RENEWED_AT_KEY } from "@/api/client";
import { getRun, listRuns, type RunSnapshot } from "@/api/runs";
import BrowserPane from "@/components/Runs/BrowserPane";
import { useViewedRunChildren } from "@/hooks/useRunChildren";
import { useRunWebSocket } from "@/hooks/useRunWebSocket";
import {
  EXPECTED_BIRTH_HEADER,
  forgetLostRunsForTest,
  isRunLost,
  knownRunBirth,
  noteRunBirth,
  noteRunRowBirth,
} from "@/lib/lostRuns";
import { createRunStore, getDefaultRunStore, RunStoreProvider } from "@/store/run";
import { ReviewScopePanel } from "@/views/PipelineBoard/ReviewScopePanel";

const A = "BIRTHAAAAAAAAAAAAAAAAAAAAA";
const B = "BIRTHBBBBBBBBBBBBBBBBBBBBB";

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
  closed = false;
  constructor(public url: string) {
    FakeWebSocket.instances.push(this);
  }
  open(): void {
    this.readyState = FakeWebSocket.OPEN;
    this.onopen?.(new Event("open"));
  }
  send(): void {}
  close(): void {
    this.closed = true;
    this.readyState = FakeWebSocket.CLOSED;
  }
  message(data: unknown): void {
    this.onmessage?.(new MessageEvent("message", { data: JSON.stringify(data) }));
  }
  serverClose(code: number, reason: string): void {
    this.readyState = FakeWebSocket.CLOSED;
    this.onclose?.(new CloseEvent("close", { code, reason }));
  }
}

interface Sent {
  url: string;
  headers: Headers;
}
let sent: Sent[] = [];
let answer: (req: Sent) => Response | Promise<Response> = () => json(200, {});

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

function snapshot(runId: string, birth: string | undefined, lastSeq = 0): RunSnapshot {
  return {
    run: { id: runId, workflow_name: "wf", status: "finished", ...(birth === undefined ? {} : { birth_id: birth }) },
    executions: [],
    last_seq: lastSeq,
  } as unknown as RunSnapshot;
}

async function flush(): Promise<void> {
  await act(async () => {
    for (let i = 0; i < 8; i++) await Promise.resolve();
  });
}

function onPage(path: string): void {
  window.history.replaceState(null, "", path);
}

beforeEach(() => {
  forgetLostRunsForTest();
  onPage("/");
  localStorage.clear();
  localStorage.setItem(SESSION_RENEWED_AT_KEY, "-60000");
  sent = [];
  answer = () => json(200, {});
  FakeWebSocket.instances.length = 0;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
      const req: Sent = { url: String(input), headers: new Headers(init?.headers) };
      sent.push(req);
      return answer(req);
    }),
  );
  vi.stubGlobal("WebSocket", FakeWebSocket);
});

afterEach(() => {
  cleanup();
  onPage("/");
  localStorage.clear();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe("R4-C1: a row pins the view it opens, and never loses a run", () => {
  it("a later poll's row replaces an earlier row's birth; the fresh row opens the run", async () => {
    answer = () => json(200, { runs: [{ id: "x", birth_id: A, status: "running" }] });
    await listRuns();
    answer = () => json(200, { runs: [{ id: "x", birth_id: B, status: "running" }] });
    await listRuns();
    expect(isRunLost("", "x")).toBe(false);
    expect(knownRunBirth("", "x")).toBe(B);
    answer = () => json(200, snapshot("x", B));
    await getRun("x");
    expect(sent[sent.length - 1]?.headers.get(EXPECTED_BIRTH_HEADER)).toBe(B);
    expect(isRunLost("", "x")).toBe(false);
  });

  it("a listing landing after a view adopted the successor changes nothing", async () => {
    let release!: (r: Response) => void;
    answer = (req) =>
      req.url.includes("/runs/x") ? json(200, snapshot("x", B)) : new Promise<Response>((res) => (release = res));
    const listing = listRuns({ limit: 50 });
    await flush();
    await getRun("x");
    release(json(200, { runs: [{ id: "x", birth_id: A, status: "running" }] }));
    await listing;
    expect(isRunLost("", "x")).toBe(false);
    expect(knownRunBirth("", "x")).toBe(B);
  });

  it("a snapshot of another birth than the row the view opened from is lost", () => {
    noteRunRowBirth("", "x", A, 1);
    expect(noteRunBirth("", "x", B)).toBe(false);
    expect(isRunLost("", "x")).toBe(true);
  });
});

describe("R4-C4: a snapshot's birth is keyed by the store it was read from", () => {
  it("getRun keys it by the store its request named, though the page moved to another store meanwhile", async () => {
    onPage("/runs/m");
    let release!: (r: Response) => void;
    answer = () => new Promise<Response>((res) => (release = res));
    const pending = getRun("m");
    await flush();
    onPage("/runs/m?store=%2Fhome%2Fu%2Fother");
    release(json(200, snapshot("m", A)));
    const snap = await pending;
    expect(knownRunBirth("", "m")).toBe(A);
    // The run store keys it by the store the snapshot is tagged with.
    forgetLostRunsForTest();
    getDefaultRunStore().getState().applySnapshot(snap);
    expect(knownRunBirth("", "m")).toBe(A);
  });

  it("a socket's snapshot is keyed by the store it dialed", async () => {
    onPage("/runs/s");
    const store = createRunStore();
    const wrapper = ({ children }: { children: ReactNode }) => createElement(RunStoreProvider, { store, children });
    renderHook(() => useRunWebSocket("s"), { wrapper });
    await flush();
    const ws = FakeWebSocket.instances.find((s) => s.url.includes("/ws/runs/s"));
    expect(ws?.url).not.toContain("store=");
    act(() => ws?.open());
    onPage("/runs/s?store=%2Fs2");
    act(() => ws?.message({ type: "snapshot", payload: snapshot("s", A) }));
    expect(knownRunBirth("", "s")).toBe(A);
  });
});

describe("R4-A5: a malformed-expectation close is not redialed", () => {
  it("the run socket stays closed", async () => {
    vi.useFakeTimers();
    const store = createRunStore();
    const wrapper = ({ children }: { children: ReactNode }) => createElement(RunStoreProvider, { store, children });
    renderHook(() => useRunWebSocket("r"), { wrapper });
    await flush();
    const ws = FakeWebSocket.instances.find((s) => s.url.includes("/ws/runs/r"));
    act(() => ws?.open());
    vi.spyOn(console, "error").mockImplementation(() => undefined);
    act(() => ws?.serverClose(1000, "invalid_expected_birth: an expected birth is \"historical\" or …"));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(120_000);
    });
    expect(FakeWebSocket.instances.filter((s) => s.url.includes("/ws/runs/r"))).toHaveLength(1);
    expect(store.getState().wsState).toBe("closed");
    expect(isRunLost("", "r")).toBe(false);
  });
});

describe("R4-C3: what waits for a birth, and what rebuilds on one", () => {
  it("the children poll waits until the page knows the run's birth, then carries it", async () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const wrapper = ({ children }: { children: ReactNode }) => createElement(QueryClientProvider, { client: qc }, children);
    answer = () => json(200, { runs: [] });
    renderHook(() => useViewedRunChildren("p", { snapshotLoaded: false, lost: false }), { wrapper });
    await flush();
    expect(sent.filter((s) => s.url.includes("/runs/p/children"))).toHaveLength(0);
    act(() => noteRunRowBirth("", "p", A, 1));
    await waitFor(() => expect(sent.filter((s) => s.url.includes("/runs/p/children"))).toHaveLength(1));
    expect(sent[0]?.headers.get(EXPECTED_BIRTH_HEADER)).toBe(A);
  });

  it("the browser pane's screenshot URL is rebuilt once the birth is learned", async () => {
    getDefaultRunStore().setState({
      browser: { ...getDefaultRunStore().getState().browser, screenshots: [{ seq: 1, attachmentName: "shot.png" }] },
    } as never);
    render(createElement(BrowserPane, { runId: "bp", scrubSeq: 5 }));
    const img = () => screen.getByAltText("Screenshot at seq 1") as HTMLImageElement;
    expect(img().getAttribute("src")).not.toContain("expected_birth");
    act(() => noteRunBirth("", "bp", A));
    expect(img().getAttribute("src")).toContain(`expected_birth=${A}`);
  });

  it("the review scope's media URL is rebuilt once the birth is learned", async () => {
    answer = (req) =>
      req.url.includes("/runs/rv/review/scope")
        ? json(200, {
            run_id: "rv",
            gate_seq: 3,
            base_ref: "a",
            head_ref: "b",
            available: true,
            groups: [{ node_id: "n", files: [{ path: "clip.mp3", status: "A", added: 0, deleted: 0 }] }],
            total_files: 1,
          })
        : json(200, {});
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(createElement(QueryClientProvider, { client: qc }, createElement(ReviewScopePanel, { runId: "rv", pauseKey: "k" })));
    fireEvent.click(await screen.findByTitle(/clip\.mp3/));
    const audio = () => document.querySelector("audio") as HTMLAudioElement;
    await waitFor(() => expect(audio()).toBeTruthy());
    expect(audio().getAttribute("src")).not.toContain("expected_birth");
    act(() => noteRunBirth("", "rv", A));
    expect(audio().getAttribute("src")).toContain(`expected_birth=${A}`);
  });
});
