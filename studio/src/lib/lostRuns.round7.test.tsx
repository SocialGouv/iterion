// @vitest-environment jsdom
// Round 7 of the expected birth: the clicked row's birth travels with the
// navigation to the run view — history state in-page (R7-2), the
// expected_birth parameter of a full page load such as the desktop's
// cross-daemon link (R7-3) — and is pinned before the view's first request: a
// poll listing a successor under the ID does not change what the view
// expects. Global fetch is the witness; the server is the stub's answers.
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createElement } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { SESSION_RENEWED_AT_KEY } from "@/api/client";
import { getRun, listRuns, type RunSnapshot, type RunSummary } from "@/api/runs";
import RunsPanel, { fullPageRunPath } from "@/components/Home/RunsPanel";
import { deriveWsUrl } from "@/hooks/runWs/url";
import {
  EXPECTED_BIRTH_HEADER,
  forgetLostRunsForTest,
  isRunLost,
  LostRunError,
  NAVIGATION_BIRTH_STATE,
} from "@/lib/lostRuns";

const A = "BIRTHAAAAAAAAAAAAAAAAAAAAA";
const B = "BIRTHBBBBBBBBBBBBBBBBBBBBB";

interface Sent {
  url: string;
  headers: Headers;
}
let sent: Sent[] = [];
let answer: (req: Sent) => Response | Promise<Response> = () => json(200, {});

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

function snapshot(runId: string, birth: string): RunSnapshot {
  return { run: { id: runId, workflow_name: "wf", status: "running", birth_id: birth }, executions: [], last_seq: 0 } as unknown as RunSnapshot;
}

// theServer answers as a server whose run x is now the successor B: an
// expectation of A is refused 409, anything else gets B's snapshot.
function theServer(req: Sent): Response {
  if (new URL(req.url, "http://studio.test").pathname.endsWith("/runs/x")) {
    return req.headers.get(EXPECTED_BIRTH_HEADER) === A
      ? json(409, { error: "replaced", error_code: "run_origin_lost", run_id: "x" })
      : json(200, snapshot("x", B));
  }
  if (req.url.includes("/runs/global-active")) return json(200, { runs: [] });
  return json(200, { runs: [{ id: "x", birth_id: B, status: "running" }] });
}

beforeEach(() => {
  forgetLostRunsForTest();
  window.history.replaceState(null, "", "/");
  localStorage.clear();
  localStorage.setItem(SESSION_RENEWED_AT_KEY, "-60000");
  sent = [];
  answer = () => json(200, {});
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
      const req: Sent = { url: String(input), headers: new Headers(init?.headers) };
      sent.push(req);
      return answer(req);
    }),
  );
});

afterEach(() => {
  cleanup();
  window.history.replaceState(null, "", "/");
  localStorage.clear();
  vi.unstubAllGlobals();
});

function clickRow(rows: RunSummary[], label: string): Promise<void> {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(createElement(QueryClientProvider, { client: qc }, createElement(RunsPanel, { runs: rows, loading: false, error: null })));
  return screen.findByText(label).then((el) => {
    fireEvent.click(el);
  });
}

describe("R7-2: an in-page row click pins the clicked row's birth", () => {
  const rows = [{ id: "x", birth_id: A, name: "row-x", workflow_name: "wf", status: "finished", created_at: "", updated_at: "" }] as unknown as RunSummary[];

  it("a poll listing the successor before the view's first request: the first snapshot request expects the clicked run, refused, lost", async () => {
    await clickRow(rows, "row-x");
    expect(window.location.pathname).toBe("/runs/x");
    answer = theServer;
    await listRuns();
    await expect(getRun("x")).rejects.toBeInstanceOf(LostRunError);
    expect(sent[sent.length - 1]?.headers.get(EXPECTED_BIRTH_HEADER)).toBe(A);
    expect(isRunLost("", "x")).toBe(true);
    expect((window.history.state as Record<string, unknown> | null)?.[NAVIGATION_BIRTH_STATE]).toBeUndefined();
  });

  it("a poll listing the successor once the first request left: the view's socket still expects the clicked run", async () => {
    await clickRow(rows, "row-x");
    let release!: (r: Response) => void;
    answer = () => new Promise<Response>((res) => (release = res));
    const first = getRun("x");
    const toRun = () => sent.find((s) => new URL(s.url, "http://studio.test").pathname.endsWith("/runs/x"));
    await vi.waitFor(() => expect(toRun()).toBeDefined());
    expect(toRun()?.headers.get(EXPECTED_BIRTH_HEADER)).toBe(A);
    answer = theServer;
    await listRuns();
    expect(new URL(await deriveWsUrl("x"), "http://studio.test").searchParams.get("expected_birth")).toBe(A);
    release(json(409, { error: "replaced", error_code: "run_origin_lost", run_id: "x" }));
    await expect(first).rejects.toBeInstanceOf(LostRunError);
  });
});

describe("R7-3: a full page load carries the clicked row's birth in its URL", () => {
  it("the desktop link names an own-store row's birth, none for a foreign store's", () => {
    expect(fullPageRunPath("/runs/x", "", A)).toBe(`/runs/x?expected_birth=${A}`);
    expect(fullPageRunPath("/runs/x", "", "")).toBe("/runs/x?expected_birth=historical");
    expect(fullPageRunPath("/runs/x", "/home/u/.iterion", A)).toBe(`/runs/x?store=${encodeURIComponent("/home/u/.iterion")}`);
  });

  it("the destination pins it before its first request, a poll before that request notwithstanding, and strips it from the URL", async () => {
    window.history.replaceState(null, "", `/runs/x?expected_birth=${A}`);
    answer = theServer;
    await listRuns();
    await expect(getRun("x")).rejects.toBeInstanceOf(LostRunError);
    expect(sent[sent.length - 1]?.headers.get(EXPECTED_BIRTH_HEADER)).toBe(A);
    expect(isRunLost("", "x")).toBe(true);
    expect(window.location.search).toBe("");
  });
});
