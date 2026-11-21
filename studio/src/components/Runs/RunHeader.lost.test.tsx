// @vitest-environment jsdom
//
// S2 (RunHeader): a view whose socket was lost says so. The header renders
// the origin-lost banner — French, its language named, with the reload — when
// wsState is "lost", and nothing of it otherwise. The whole chain is driven
// too: a mutation the server refuses 409 run_origin_lost marks the run lost
// (apiRequest), the run socket hook closes and turns the view lost, and the
// header mounted on that hook's state shows the banner.
import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { apiRequest, cancelScheduledRefresh, SESSION_RENEWED_AT_KEY } from "@/api/client";
import type { RunHeader as RunHeaderType } from "@/api/runs";
import { useRunWebSocket } from "@/hooks/useRunWebSocket";
import { forgetLostRunsForTest, isRunLost, LostRunError, noteRunBirth } from "@/lib/lostRuns";
import { createRunStore, RunStoreProvider, useRunStore } from "@/store/run";

vi.mock("wouter", () => ({
  useLocation: () => ["/", vi.fn()],
}));
vi.mock("@/hooks/useConfirm", () => ({
  useConfirm: () => ({ confirm: vi.fn(async () => true), dialog: <div /> }),
}));
// The dialogs and the side rows are heavy or fetch on mount and are
// irrelevant: this is about the banner the header shows for a lost socket.
vi.mock("./ForkDialog", () => ({ default: () => <div /> }));
vi.mock("./RewindDialog", () => ({ default: () => <div /> }));
vi.mock("./ResumeDialog", () => ({ default: () => <div /> }));
vi.mock("./RunShellPanel", () => ({ RunShellPanel: () => <div /> }));
vi.mock("./runHeader/AssistantWatchBadge", () => ({ default: () => <div /> }));
vi.mock("./runHeader/BackendsUsedRow", () => ({ default: () => <div /> }));
vi.mock("./runHeader/BotChip", () => ({ default: () => <div /> }));
vi.mock("./runHeader/DeploymentRow", () => ({ default: () => <div /> }));
vi.mock("./runHeader/ErrorHintRow", () => ({ default: () => <div /> }));
vi.mock("./runHeader/FallbacksUsedRow", () => ({ default: () => <div /> }));
vi.mock("./runHeader/FinalizationRow", () => ({ default: () => <div /> }));
vi.mock("./runHeader/ForkedFromRow", () => ({ default: () => <div /> }));
vi.mock("./runHeader/RunChildrenPanel", () => ({ default: () => <div /> }));
vi.mock("./runHeader/NotesRow", () => ({ default: () => <div /> }));
vi.mock("./runHeader/ResultLinksRow", () => ({ default: () => <div /> }));
vi.mock("./runHeader/DeliverablesRow", () => ({ default: () => <div /> }));
vi.mock("./runHeader/RunTagsRow", () => ({ default: () => <div /> }));
vi.mock("./runHeader/RunInformationAccordion", () => ({ default: () => <div /> }));
vi.mock("./runHeader/SourceTicketRow", () => ({ default: () => <div /> }));
vi.mock("@/components/shared/WSStatusDot", () => ({ default: () => <div /> }));

import RunHeader from "./RunHeader";

const A = "BIRTHAAAAAAAAAAAAAAAAAAAAA";
const TITLE = "Ce run a été remplacé ou supprimé, ou votre accès a changé.";

// A finished run: no live stream is expected, so the disconnect banner
// stays out of the way and the only alert is the one under test.
const run = {
  id: "r",
  status: "finished",
  name: "Demo run",
  workflow_name: "wf",
  created_at: new Date().toISOString(),
} as unknown as RunHeaderType;

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
}

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

async function flush(): Promise<void> {
  await act(async () => {
    for (let i = 0; i < 8; i++) await Promise.resolve();
  });
}

// lostBanner is the origin-lost alert, or null: found by its French title.
function lostBanner(): HTMLElement | null {
  return screen.queryAllByRole("alert").find((el) => el.textContent?.includes(TITLE)) ?? null;
}

function expectLostBanner(): void {
  const alert = lostBanner();
  expect(alert, "the origin-lost banner").not.toBeNull();
  expect(alert?.closest("[lang]")?.getAttribute("lang")).toBe("fr");
  expect(screen.getByRole("button", { name: "Recharger" })).toBeTruthy();
}

function runSocket(runId: string): FakeWebSocket {
  const ws = FakeWebSocket.instances.find((s) => s.url.includes(`/ws/runs/${runId}`));
  if (!ws) throw new Error(`no run socket for ${runId}: ${FakeWebSocket.instances.map((s) => s.url).join(", ")}`);
  return ws;
}

// LiveHeader is the header on the state the run socket hook drives, as the
// run console mounts it.
function LiveHeader() {
  useRunWebSocket(run.id);
  const wsState = useRunStore((s) => s.wsState);
  return <RunHeader run={run} active={false} wsState={wsState} />;
}

beforeEach(() => {
  forgetLostRunsForTest();
  localStorage.clear();
  localStorage.setItem(SESSION_RENEWED_AT_KEY, "-60000");
  FakeWebSocket.instances.length = 0;
  vi.stubGlobal("WebSocket", FakeWebSocket);
});

afterEach(() => {
  cleanup();
  cancelScheduledRefresh();
  localStorage.clear();
  vi.unstubAllGlobals();
});

describe("RunHeader on a lost socket", () => {
  it("shows the origin-lost banner, in French, with the reload", () => {
    render(<RunHeader run={run} active={false} wsState="lost" />);
    expectLostBanner();
  });

  it.each(["open", "connecting", "reconnecting", "closed"] as const)("shows none while the socket is %s", (state) => {
    render(<RunHeader run={run} active={false} wsState={state} />);
    expect(lostBanner()).toBeNull();
    expect(screen.queryByRole("button", { name: "Recharger" })).toBeNull();
  });

  it("shows it once a mutation is refused run_origin_lost: the 409, the hook, the header", async () => {
    noteRunBirth("", run.id, A);
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => json(409, { error: "replaced", error_code: "run_origin_lost", run_id: run.id })),
    );
    const store = createRunStore();
    render(
      <RunStoreProvider store={store}>
        <LiveHeader />
      </RunStoreProvider>,
    );
    await flush();
    const ws = runSocket(run.id);
    act(() => ws.open());
    expect(store.getState().wsState).toBe("open");
    expect(lostBanner()).toBeNull();

    let refused: unknown;
    await act(async () => {
      refused = await apiRequest(`/api/runs/${run.id}/merge`, { method: "POST" }).catch((e: unknown) => e);
    });
    expect(refused).toBeInstanceOf(LostRunError);
    expect(isRunLost("", run.id)).toBe(true);
    expect(ws.closed).toBe(true);
    expect(store.getState().wsState).toBe("lost");
    expectLostBanner();
  });
});
