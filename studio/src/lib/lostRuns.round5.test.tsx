// @vitest-environment jsdom
// Round 5 of the expected birth. Cross-store views are outside the guarantee
// and keep each route's own store behavior; a run link names a store only
// when the destination daemon does not own it (X5-2). The lost registry is
// keyed by store and run (X5-3). A store override with spaces is a store, an
// invalid one an error (X5-4). A row of an earlier-sent request never repins
// a later one's (R5-1). Global fetch and WebSocket are the witnesses; the
// server is the stub's answers.
import { act, cleanup, render, renderHook, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createElement, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { resolveAssistantContext } from "@/api/assistantContext";
import { apiRequest, SESSION_RENEWED_AT_KEY } from "@/api/client";
import {
  apiURL,
  crossDaemonRunStore,
  getRun,
  getRunWorkflow,
  getSessionBoard,
  listRuns,
  loadEvents,
  type GlobalActiveRun,
  type RunSnapshot,
} from "@/api/runs";
import RunsPanel from "@/components/Home/RunsPanel";
import { useRunWebSocket } from "@/hooks/useRunWebSocket";
import {
  EXPECTED_BIRTH_HEADER,
  forgetLostRunsForTest,
  isRunLost,
  knownRunBirth,
  markRunLost,
  noteRunBirth,
  runFetch,
} from "@/lib/lostRuns";
import { InvalidStoreOverrideError } from "@/lib/storeOverride";
import { createRunStore, RunStoreProvider } from "@/store/run";

const A = "BIRTHAAAAAAAAAAAAAAAAAAAAA";
const B = "BIRTHBBBBBBBBBBBBBBBBBBBBB";
const FOREIGN = "/home/u/.iterion";

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
}

interface Sent {
  url: string;
  headers: Headers;
  body?: string;
}
let sent: Sent[] = [];
let answer: (req: Sent) => Response | Promise<Response> = () => json(200, {});

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

function snapshot(runId: string, birth: string): RunSnapshot {
  return {
    run: { id: runId, workflow_name: "wf", status: "finished", birth_id: birth },
    executions: [],
    last_seq: 0,
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

function storeOf(req: Sent | undefined): string | null {
  return new URL(req?.url ?? "", "http://studio.test").searchParams.get("store");
}

function socketTo(runId: string): FakeWebSocket | undefined {
  return FakeWebSocket.instances.find((s) => new URL(s.url, "http://studio.test").pathname.endsWith(`/ws/runs/${runId}`));
}

function dial(runId: string) {
  const store = createRunStore();
  const wrapper = ({ children }: { children: ReactNode }) => createElement(RunStoreProvider, { store, children });
  renderHook(() => useRunWebSocket(runId), { wrapper });
  return store;
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
      const req: Sent = { url: String(input), headers: new Headers(init?.headers), body: typeof init?.body === "string" ? init.body : undefined };
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
  vi.unstubAllGlobals();
});

describe("X5-2: a foreign store's view is outside the guarantee", () => {
  it("its run's store-reading routes name the store, the others none, none carries an expectation; another run keeps its own", async () => {
    onPage(`/runs/r?store=${encodeURIComponent(FOREIGN)}`);
    noteRunBirth("", "r", B); // a run of the daemon's own store under the same ID
    noteRunBirth("", "a1", A); // the chat dock's assistant run
    answer = (req) => {
      const path = new URL(req.url, "http://studio.test").pathname;
      if (path.endsWith("/runs/r")) return json(200, snapshot("r", "FOREIGNBIRTH"));
      if (path.endsWith("/runs/a1")) return json(200, snapshot("a1", A));
      return json(200, {});
    };
    await getRun("r");
    await loadEvents("r");
    await getRunWorkflow("r");
    await getSessionBoard("r");
    const storeRoutes = sent.splice(0);
    await apiRequest("/api/runs/r/tags");
    await runFetch("/api/runs/r/log");
    const ownRoutes = sent.splice(0);
    await getRun("a1");
    const other = sent.splice(0);

    for (const req of storeRoutes) {
      expect(storeOf(req), req.url).toBe(FOREIGN);
      expect(req.headers.get(EXPECTED_BIRTH_HEADER), req.url).toBeNull();
    }
    for (const req of ownRoutes) {
      expect(storeOf(req), req.url).toBeNull();
      expect(req.headers.get(EXPECTED_BIRTH_HEADER), req.url).toBeNull();
    }
    expect(storeOf(other[0])).toBeNull();
    expect(other[0]?.headers.get(EXPECTED_BIRTH_HEADER)).toBe(A);
    expect(new URL(apiURL("/runs/r/log"), "http://studio.test").search).toBe("");
    expect(new URL(apiURL("/runs/a1/log"), "http://studio.test").searchParams.get("expected_birth")).toBe(A);
    expect(knownRunBirth("", "r")).toBe(B);
    expect(isRunLost("", "r") || isRunLost(FOREIGN, "r")).toBe(false);
  });

  it("its run's socket names the store and no expectation; another run's names its birth and no store", async () => {
    onPage(`/runs/r?store=${encodeURIComponent(FOREIGN)}`);
    noteRunBirth("", "r", B);
    noteRunBirth("", "a1", A);
    dial("r");
    dial("a1");
    await flush();
    const foreign = new URL(socketTo("r")?.url ?? "", "http://studio.test").searchParams;
    const other = new URL(socketTo("a1")?.url ?? "", "http://studio.test").searchParams;
    expect(foreign.get("store")).toBe(FOREIGN);
    expect(foreign.get("expected_birth")).toBeNull();
    expect(other.get("store")).toBeNull();
    expect(other.get("expected_birth")).toBe(A);
  });

  it("a request body naming its run carries no birth for it; another run's carries its own", async () => {
    onPage(`/runs/r?store=${encodeURIComponent(FOREIGN)}`);
    noteRunBirth("", "r", B);
    noteRunBirth("", "a1", A);
    answer = () => json(200, { references: [] });
    await resolveAssistantContext(["run/r", "run/a1"]);
    expect((JSON.parse(sent[0]?.body ?? "{}") as { references?: string[] }).references).toEqual(["run/r", `run/a1@${A}`]);
  });

  it("no birth is adopted for it: a later snapshot of another birth is shown, not lost", () => {
    onPage(`/runs/r?store=${encodeURIComponent(FOREIGN)}`);
    const store = createRunStore();
    store.getState().applySnapshot(snapshot("r", A));
    store.getState().applySnapshot({ ...snapshot("r", B), last_seq: 2 });
    expect(store.getState().wsState).not.toBe("lost");
    expect(store.getState().snapshot?.run.birth_id).toBe(B);
  });

  it("a global-active row links with a store only when the daemon does not own it", async () => {
    const rows: GlobalActiveRun[] = [
      { id: "own", workflow_name: "own-wf", status: "running", created_at: "", updated_at: "", store_path: "/home/u/.iterion/projects/p", own_store: true },
      { id: "far", workflow_name: "far-wf", status: "running", created_at: "", updated_at: "", store_path: FOREIGN },
    ];
    answer = () => json(200, { runs: rows });
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(createElement(QueryClientProvider, { client: qc }, createElement(RunsPanel, { runs: [], loading: false, error: null })));
    const own = (await screen.findByText("own-wf")).closest("a");
    const far = (await screen.findByText("far-wf")).closest("a");
    expect(own?.getAttribute("href")).toBe("/runs/own");
    expect(far?.getAttribute("href")).toBe(`/runs/far?store=${encodeURIComponent(FOREIGN)}`);
  });

  it("the desktop names the store only when the destination daemon does not own it", () => {
    const own = { store_path: "/p", own_store: true } as GlobalActiveRun;
    const far = { store_path: FOREIGN } as GlobalActiveRun;
    // Another daemon resolved for the run's store serves it as its own.
    expect(crossDaemonRunStore(far, "http://127.0.0.1:2", "http://127.0.0.1:1")).toBe("");
    // This daemon: the row says.
    expect(crossDaemonRunStore(own, "http://127.0.0.1:1/", "http://127.0.0.1:1")).toBe("");
    expect(crossDaemonRunStore(far, "http://127.0.0.1:1/", "http://127.0.0.1:1")).toBe(FOREIGN);
    // Unknown current daemon: the store is named.
    expect(crossDaemonRunStore(own, "http://127.0.0.1:1", "")).toBe("/p");
  });
});

describe("X5-3: the lost registry is keyed by store and run", () => {
  it("a run lost in one store leaves the same ID in another store reachable, and its view's socket open", async () => {
    onPage(`/runs/r?store=${encodeURIComponent(FOREIGN)}`);
    const view = dial("r");
    await flush();
    const ws = socketTo("r");
    act(() => ws?.open());
    act(() => markRunLost("", "r"));
    expect(ws?.closed).toBe(false);
    expect(view.getState().wsState).toBe("open");
    answer = () => json(200, snapshot("r", A));
    await expect(getRun("r")).resolves.toBeDefined();
    expect(storeOf(sent[sent.length - 1])).toBe(FOREIGN);

    act(() => markRunLost(FOREIGN, "r"));
    expect(ws?.closed).toBe(true);
    expect(view.getState().wsState).toBe("lost");
    onPage("/runs/other");
    sent.length = 0;
    await expect(apiRequest(`/api/runs/r?store=${encodeURIComponent("/elsewhere")}`)).resolves.toBeDefined();
    expect(sent).toHaveLength(1);
  });
});

describe("R6-F5: a store and a run ID never collide in the registry", () => {
  it("run c of store …/a|b and run b|c of store …/a are two runs", () => {
    markRunLost("/x/projects/a|b", "c");
    expect(isRunLost("/x/projects/a", "b|c")).toBe(false);
    forgetLostRunsForTest();
    markRunLost("/x/projects/a", "b|c");
    expect(isRunLost("/x/projects/a|b", "c")).toBe(false);
    expect(isRunLost("/x/projects/a", "b|c")).toBe(true);
  });
});

describe("X5-4: the store override is a store path or an error", () => {
  it("a present but empty override fails the view, never reads the daemon's own run", async () => {
    onPage("/runs/r?store=");
    await expect(getRun("r")).rejects.toBeInstanceOf(InvalidStoreOverrideError);
    expect(sent).toHaveLength(0);
  });

  it("a path with spaces is a store", async () => {
    onPage(`/runs/r?store=${encodeURIComponent("/home/u/My Work/.iterion")}`);
    answer = () => json(200, snapshot("r", A));
    await expect(getRun("r")).resolves.toBeDefined();
    expect(storeOf(sent[0])).toBe("/home/u/My Work/.iterion");
  });

  it("an invalid override fails the view, never reads the daemon's own run", async () => {
    onPage(`/runs/r?store=${encodeURIComponent("/home/u/../etc")}`);
    await expect(getRun("r")).rejects.toBeInstanceOf(InvalidStoreOverrideError);
    expect(sent).toHaveLength(0);
    vi.spyOn(console, "error").mockImplementation(() => undefined);
    const view = dial("r");
    await flush();
    expect(socketTo("r")).toBeUndefined();
    expect(view.getState().wsState).toBe("closed");
  });
});

describe("R5-1: a row of an earlier-sent request never repins a later one's", () => {
  it("an older listing landing after a newer one leaves the newer row's birth", async () => {
    let releaseOld!: (r: Response) => void;
    let calls = 0;
    answer = () => {
      calls++;
      return calls === 1
        ? new Promise<Response>((res) => (releaseOld = res))
        : json(200, { runs: [{ id: "x", birth_id: B, status: "running" }] });
    };
    const older = listRuns();
    await flush();
    await listRuns();
    releaseOld(json(200, { runs: [{ id: "x", birth_id: A, status: "running" }] }));
    await older;
    expect(knownRunBirth("", "x")).toBe(B);
    answer = () => json(200, snapshot("x", B));
    await getRun("x");
    expect(sent[sent.length - 1]?.headers.get(EXPECTED_BIRTH_HEADER)).toBe(B);
    expect(isRunLost("", "x")).toBe(false);
  });
});
