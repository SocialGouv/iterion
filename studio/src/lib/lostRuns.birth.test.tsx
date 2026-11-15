// @vitest-environment jsdom
// The expected birth (slice 1b): the page sends back the birth of every run
// it learned — the X-Iterion-Expected-Birth header on a fetch, expected_birth
// on a URL the browser loads by itself or a socket dial, `<x>_run_birth` in a
// body — and maps the server's refusal (409 run_origin_lost, a scope_lost
// close) to the lost-run registry. Global fetch and WebSocket are the
// witnesses; the server is the stub's answers.
import { act, cleanup, render, renderHook } from "@testing-library/react";
import { createElement, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { resolveAssistantContext } from "@/api/assistantContext";
import { apiRequest, cancelScheduledRefresh, publishExplicitTeam, SESSION_RENEWED_AT_KEY } from "@/api/client";
import {
  apiURL,
  artifactFileURL,
  attachmentURL,
  createAssistantRunWatch,
  createRun,
  getRun,
  getRunChildren,
  listGlobalActiveRuns,
  listRuns,
  runBrowserAttachURL,
  runPreviewURL,
  workspaceFileURL,
} from "@/api/runs";
import { bindWorkspaceHandoff, completeWorkspaceHandoff, createWorkspaceHandoff } from "@/api/workspaceHandoff";
import { RunShellPanel } from "@/components/Runs/RunShellPanel";
import { deriveWsUrl } from "@/hooks/runWs/url";
import { useRunWebSocket } from "@/hooks/useRunWebSocket";
import { CDPClient } from "@/lib/cdpClient";
import {
  EXPECTED_BIRTH_HEADER,
  forgetLostRunsForTest,
  isRunLost,
  knownRunBirth,
  LostRunError,
  markRunLost,
  noteRunBirth,
  runFetch,
} from "@/lib/lostRuns";
import { createRunStore, RunStoreProvider } from "@/store/run";
import type { RunSnapshot } from "@/api/runs";

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

const A = "BIRTHAAAAAAAAAAAAAAAAAAAAA";
const B = "BIRTHBBBBBBBBBBBBBBBBBBBBB";
const SCOPE_LOST = "scope_lost: the run this socket watches was replaced or deleted";

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
  closed = false;
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
    this.closed = true;
    this.readyState = FakeWebSocket.CLOSED;
  }
  serverClose(code: number, reason: string): void {
    this.readyState = FakeWebSocket.CLOSED;
    this.onclose?.(new CloseEvent("close", { code, reason }));
  }
}

interface Sent {
  url: string;
  method: string;
  headers: Headers;
  body: string;
}
let sent: Sent[] = [];
// answer decides the stub server's reply to one request.
let answer: (req: Sent) => Response = () => json(200, {});

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

// openShell mounts the shell drawer on runId: first on another run, so the
// drawer's host exists when the effect for runId runs, then waits for its
// socket (xterm loads lazily).
async function openShell(runId: string) {
  const view = render(createElement(RunShellPanel, { open: true, onOpenChange: () => undefined, runId: "drawer-mount" }));
  view.rerender(createElement(RunShellPanel, { open: true, onOpenChange: () => undefined, runId }));
  const ws = await vi.waitFor(async () => {
    await flush();
    const found = FakeWebSocket.instances.find((s) => s.url.includes(`/runs/${runId}/shell`));
    if (!found) throw new Error(`no shell socket for ${runId}`);
    return found;
  });
  return { view, ws };
}

beforeEach(() => {
  forgetLostRunsForTest();
  localStorage.clear();
  localStorage.setItem(SESSION_RENEWED_AT_KEY, "-60000");
  sent = [];
  answer = () => json(200, {});
  FakeWebSocket.instances.length = 0;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
      const req: Sent = {
        url: String(input),
        method: init?.method ?? "GET",
        headers: new Headers(init?.headers),
        body: typeof init?.body === "string" ? init.body : "",
      };
      sent.push(req);
      return answer(req);
    }),
  );
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
  cancelScheduledRefresh();
  publishExplicitTeam(null);
  localStorage.clear();
  vi.unstubAllGlobals();
});

describe("S1: the expected birth rides on every request addressing a known run", () => {
  it("apiRequest sends it beside the caller's headers and the JSON and team defaults, on the 401 replay too (F6)", async () => {
    noteRunBirth("", "r", A);
    publishExplicitTeam("tb");
    let renewed = false;
    answer = (req) => {
      if (req.url.endsWith("/auth/refresh")) {
        renewed = true;
        return json(200, { user: { id: "u1" } });
      }
      return renewed ? json(200, { ok: true }) : json(401, { error: "token expired" });
    };
    await apiRequest("/api/runs/r/tags", { method: "PUT", headers: { "X-Probe": "1" }, body: "{}" });
    const calls = sent.filter((s) => s.url === "/api/runs/r/tags");
    expect(calls).toHaveLength(2);
    for (const call of calls) {
      expect(call.headers.get(EXPECTED_BIRTH_HEADER)).toBe(A);
      expect(call.headers.get("X-Probe")).toBe("1");
      expect(call.headers.get("Content-Type")).toBe("application/json");
      expect(call.headers.get("X-Iterion-Team")).toBe("tb");
    }
  });

  it("sends the sentinel for a historical run, nothing for an unknown run or a non-run path", async () => {
    noteRunBirth("", "h", "");
    await apiRequest("/api/runs/h");
    await apiRequest("/api/runs/unknown");
    await apiRequest("/api/runs/global-active");
    expect(sent.map((s) => s.headers.get(EXPECTED_BIRTH_HEADER))).toEqual(["historical", null, null]);
  });

  it("sends none for a request naming a foreign store: nothing is noted for one", async () => {
    noteRunBirth("/home/u/.iterion", "r", A);
    noteRunBirth("", "r", B);
    await apiRequest("/api/runs/r?store=%2Fhome%2Fu%2F.iterion");
    await apiRequest("/api/runs/r");
    expect(sent.map((s) => s.headers.get(EXPECTED_BIRTH_HEADER))).toEqual([null, B]);
  });

  it("runFetch sends it, and sends no header when the URL already names it", async () => {
    noteRunBirth("", "r", A);
    await runFetch("/api/runs/r/log", {});
    await runFetch(apiURL("/runs/r/log"), { headers: { Accept: "text/plain" } });
    expect(sent[0]?.headers.get(EXPECTED_BIRTH_HEADER)).toBe(A);
    expect(sent[1]?.url).toContain(`expected_birth=${A}`);
    expect(sent[1]?.headers.get(EXPECTED_BIRTH_HEADER)).toBeNull();
    expect(sent[1]?.headers.get("Accept")).toBe("text/plain");
  });

  it("puts it on every URL the browser loads by itself", () => {
    noteRunBirth("", "r", A);
    const urls = [
      apiURL("/runs/r/log?from=0"),
      attachmentURL("r", "shot.png"),
      artifactFileURL("r", "dir/out.md", { download: true }),
      workspaceFileURL("r", "media/clip.mp4", { gate: 3 }),
      runPreviewURL("r", "http://localhost:3000/a?b=1"),
      runBrowserAttachURL("r"),
    ];
    for (const url of urls) {
      const parsed = new URL(url, "http://studio.test");
      expect(parsed.searchParams.get("expected_birth"), url).toBe(A);
    }
    const param = (url: string | undefined, name: string) => new URL(url ?? "", "http://studio.test").searchParams.get(name);
    expect(param(urls[2], "download")).toBe("1");
    expect(param(urls[0], "from")).toBe("0");
    expect(param(urls[4], "target")).toBe("http://localhost:3000/a?b=1");
    expect(attachmentURL("other", "x.png")).not.toContain("expected_birth");
  });

  it("puts it on the run socket, the shell and the CDP dial", async () => {
    noteRunBirth("", "r", A);
    expect(await deriveWsUrl("r")).toContain(`expected_birth=${A}`);
    const { ws: shell } = await openShell("r");
    const cdp = new CDPClient({ runId: "r", sessionId: "s1" });
    const connecting = cdp.connect();
    await flush();
    FakeWebSocket.instances.find((ws) => ws.url.includes("/browser/cdp"))?.open();
    await connecting;
    const pump = FakeWebSocket.instances.find((ws) => ws.url.includes("/browser/cdp"));
    expect(shell.url).toContain(`expected_birth=${A}`);
    expect(pump?.url).toContain(`expected_birth=${A}`);
  });
});

describe("S2: a 409 run_origin_lost marks the run lost", () => {
  it("on a path-addressed request: LostRunError, and nothing more leaves for that run", async () => {
    noteRunBirth("", "r", A);
    answer = () => json(409, { error: "replaced", error_code: "run_origin_lost", run_id: "r" });
    await expect(apiRequest("/api/runs/r/merge", { method: "POST" })).rejects.toBeInstanceOf(LostRunError);
    expect(isRunLost("", "r")).toBe(true);
    const before = sent.length;
    await expect(apiRequest("/api/runs/r")).rejects.toBeInstanceOf(LostRunError);
    expect(sent.length).toBe(before);
  });

  it("on a body-carried run: the run the body named", async () => {
    answer = () => json(409, { error: "replaced", error_code: "run_origin_lost", run_id: "assistant-1" });
    const err = await apiRequest("/api/runs/target/assistant-watches", { method: "POST", body: "{}" }).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(LostRunError);
    expect((err as LostRunError).runId).toBe("assistant-1");
    expect(isRunLost("", "assistant-1")).toBe(true);
    expect(isRunLost("", "target")).toBe(false);
  });

  it("through runFetch and the workspace handoff transport", async () => {
    noteRunBirth("", "r", A);
    answer = () => json(409, { error: "replaced", error_code: "run_origin_lost" });
    await expect(runFetch("/api/runs/r/log")).rejects.toBeInstanceOf(LostRunError);
    expect(isRunLost("", "r")).toBe(true);
    answer = () => json(409, { error: "replaced", error_code: "run_origin_lost", run_id: "dest" });
    await expect(bindWorkspaceHandoff("h1", "dest")).rejects.toBeInstanceOf(LostRunError);
    expect(isRunLost("", "dest")).toBe(true);
  });

  it("another 409 stays an API error", async () => {
    answer = () => json(409, { error: "cross_store_readonly", error_code: "cross_store_readonly" });
    const err = await apiRequest("/api/runs/r/cancel", { method: "POST" }).catch((e: unknown) => e);
    expect(err).not.toBeInstanceOf(LostRunError);
    expect(isRunLost("", "r")).toBe(false);
  });

  it("closes the view's socket and shows it lost, whoever detected the loss", async () => {
    const store = createRunStore();
    const wrapper = ({ children }: { children: ReactNode }) => createElement(RunStoreProvider, { store, children });
    renderHook(() => useRunWebSocket("r"), { wrapper });
    await flush();
    const ws = FakeWebSocket.instances.find((s) => s.url.includes("/ws/runs/r"));
    act(() => ws?.open());
    expect(store.getState().wsState).toBe("open");
    act(() => markRunLost("", "r"));
    expect(ws?.closed).toBe(true);
    expect(store.getState().wsState).toBe("lost");
    const dials = FakeWebSocket.instances.length;
    await flush();
    expect(FakeWebSocket.instances.length).toBe(dials);
  });
});

describe("S3: the first birth is adopted, another one is lost", () => {
  it("in the run store: a snapshot of another birth is dropped and the view lost", () => {
    const store = createRunStore();
    store.getState().applySnapshot(snapshot("r", A, 1));
    expect(knownRunBirth("", "r")).toBe(A);
    expect(store.getState().snapshot?.run.birth_id).toBe(A);
    store.getState().applySnapshot(snapshot("r", B, 5));
    expect(store.getState().snapshot?.last_seq).toBe(1);
    expect(store.getState().wsState).toBe("lost");
    expect(isRunLost("", "r")).toBe(true);
  });

  it("a server that predates birth exposure records nothing", () => {
    const store = createRunStore();
    store.getState().applySnapshot(snapshot("old", undefined, 1));
    store.getState().applySnapshot(snapshot("old", undefined, 2));
    expect(knownRunBirth("", "old")).toBeUndefined();
    expect(store.getState().snapshot?.last_seq).toBe(2);
    expect(isRunLost("", "old")).toBe(false);
  });

  it("getRun adopts the first snapshot's birth and refuses another", async () => {
    answer = () => json(200, snapshot("r", A));
    await getRun("r");
    expect(knownRunBirth("", "r")).toBe(A);
    answer = () => json(200, snapshot("r", B));
    await expect(getRun("r")).rejects.toBeInstanceOf(LostRunError);
    expect(sent[1]?.headers.get(EXPECTED_BIRTH_HEADER)).toBe(A);
    expect(isRunLost("", "r")).toBe(true);
  });
});

describe("F4: a run row pins the view opened from it", () => {
  it("a row's birth rides on the view's first snapshot and socket; a replacement since the listing is lost", async () => {
    answer = () => json(200, { runs: [{ id: "r", birth_id: A, workflow_name: "wf", status: "finished" }] });
    await listRuns();
    expect(await deriveWsUrl("r")).toContain(`expected_birth=${A}`);
    // The run was deleted and recreated between the listing and the click:
    // the server refuses the expectation the row pinned.
    answer = (req) =>
      req.headers.get(EXPECTED_BIRTH_HEADER) === A
        ? json(409, { error: "replaced", error_code: "run_origin_lost", run_id: "r" })
        : json(200, snapshot("r", B));
    await expect(getRun("r")).rejects.toBeInstanceOf(LostRunError);
    expect(isRunLost("", "r")).toBe(true);
  });

  it("children rows and the global-active rows of the daemon's own store are noted too; a foreign store's are not", async () => {
    answer = () => json(200, { runs: [{ id: "kid", birth_id: A, status: "finished" }] });
    await getRunChildren("parent");
    expect(knownRunBirth("", "kid")).toBe(A);
    answer = () =>
      json(200, {
        runs: [
          { id: "g", birth_id: B, store_path: "/home/u/.iterion/projects/p", own_store: true, status: "running" },
          { id: "f", birth_id: A, store_path: "/home/u/.iterion", status: "running" },
        ],
      });
    await listGlobalActiveRuns();
    expect(knownRunBirth("", "g")).toBe(B);
    expect(knownRunBirth("", "f")).toBeUndefined();
    expect(knownRunBirth("/home/u/.iterion", "f")).toBeUndefined();
  });
});

describe("S5: a run carried in a body carries its birth", () => {
  it("assistant watch, delegated launch, workspace handoffs", async () => {
    noteRunBirth("", "assistant", A);
    noteRunBirth("", "src", B);
    noteRunBirth("", "dest", "");
    answer = () => json(200, {});
    await createAssistantRunWatch("target", { assistant_run_id: "assistant", mode: "propose" });
    await createAssistantRunWatch("target", { assistant_run_id: "unknown", mode: "propose" });
    await createRun({ file_path: "bots/x/main.bot", source_run_id: "src", instructions: "fix" });
    await createWorkspaceHandoff("dest-project", "summary", "src");
    await bindWorkspaceHandoff("h1", "dest");
    await completeWorkspaceHandoff("h1", "dest", { status: "completed", summary: "ok" });
    const bodies = sent.map((s) => JSON.parse(s.body || "{}") as Record<string, unknown>);
    expect(bodies[0]?.assistant_run_birth).toBe(A);
    expect(bodies[1]).not.toHaveProperty("assistant_run_birth");
    expect(bodies[2]?.source_run_birth).toBe(B);
    expect(bodies[3]?.source_run_birth).toBe(B);
    expect(bodies[4]?.destination_run_birth).toBe("historical");
    expect(bodies[5]?.destination_run_birth).toBe("historical");
  });

  it("typed context references spell the birth and map the answer back", async () => {
    noteRunBirth("", "r", A);
    answer = (req) => {
      const refs = (JSON.parse(req.body) as { references: string[] }).references;
      return json(200, {
        references: refs.map((reference) =>
          reference.startsWith("run/r@") ? { reference, resolved: false, reason: "run_origin_lost" } : { reference, resolved: true },
        ),
      });
    };
    const out = await resolveAssistantContext(["run/r", "node/r/n1", "run/other", "card/native:1"]);
    expect((JSON.parse(sent[0]?.body ?? "{}") as { references?: string[] }).references).toEqual([
      `run/r@${A}`,
      `node/r@${A}/n1`,
      "run/other",
      "card/native:1",
    ]);
    expect(out.references.map((r) => r.reference)).toEqual(["run/r", "node/r/n1", "run/other", "card/native:1"]);
    expect(isRunLost("", "r")).toBe(true);
  });
});

describe("F8: the shell and the CDP pump show a lost run on a scope_lost close", () => {
  it("the shell drawer names the loss and marks the run", async () => {
    const { view, ws } = await openShell("r");
    act(() => ws.open());
    act(() => ws.serverClose(1000, SCOPE_LOST));
    await flush();
    expect(isRunLost("", "r")).toBe(true);
    expect(view.baseElement.textContent).toContain("was replaced or deleted");
  });

  it("the CDP client rejects with LostRunError and marks the run", async () => {
    const cdp = new CDPClient({ runId: "r", sessionId: "s1" });
    const connecting = cdp.connect();
    await flush();
    const ws = FakeWebSocket.instances.find((s) => s.url.includes("/browser/cdp"));
    ws?.open();
    await connecting;
    const pending = cdp.send("Target.getTargets");
    await flush();
    ws?.serverClose(1000, SCOPE_LOST);
    await expect(pending).rejects.toBeInstanceOf(LostRunError);
    await expect(cdp.send("Page.navigate")).rejects.toBeInstanceOf(LostRunError);
    expect(isRunLost("", "r")).toBe(true);
  });
});
