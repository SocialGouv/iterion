// @vitest-environment jsdom
//
// The Test pane is the second place a bot is launched from, and BotHome
// renders it only while it is open: closing it, or leaving the page, while it
// says "Starting…" unmounts it (#3416, rva round 4). Its launch goes through
// the same helper as the launch form's (lib/sendLaunch), under the same key:
// the answer of a pane that is gone is announced, the pane opened again is
// still held, and the form and the pane of one bot hold each other.
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const runsApi = vi.hoisted(() => ({ createRun: vi.fn() }));
vi.mock("@/api/runs", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/runs")>()),
  createRun: runsApi.createRun,
}));
const navigate = vi.hoisted(() => vi.fn());
// The session's team, which the pane's target carries; null is no session.
const authBox = vi.hoisted(() => ({ value: null as null | { activeTeamID: string } }));
vi.mock("@/auth/AuthContext", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/auth/AuthContext")>()),
  useMaybeAuth: () => authBox.value,
}));
vi.mock("wouter", async (importOriginal) => ({
  ...(await importOriginal<typeof import("wouter")>()),
  useLocation: () => ["/bots/nestor", navigate],
}));

import { apiErrorFrom } from "@/api/client";
import { botLaunchKey } from "@/lib/sendLaunch";
import { useLaunchesInFlight } from "@/store/launchesInFlight";
import { useUIStore } from "@/store/ui";

import TestRunPane from "./TestRunPane";

const pane = () => (
  <QueryClientProvider client={new QueryClient()}>
    <TestRunPane launch={{ by: "bot", bot: "nestor" }} vars={[]} />
  </QueryClientProvider>
);
const button = () => screen.getByRole("button", { name: /^(Test|Starting…)$/ }) as HTMLButtonElement;
const inFlight = () => useLaunchesInFlight.getState().byTarget;
const toasts = () => useUIStore.getState().toasts;

// A launch whose answer the test gives later.
function pendingRun() {
  let resolve!: (r: { run_id: string }) => void;
  let reject!: (e: unknown) => void;
  runsApi.createRun.mockImplementationOnce(
    () =>
      new Promise((res, rej) => {
        resolve = res;
        reject = rej;
      }),
  );
  // Read when the answer is given: the promise exists once the pane asked.
  return {
    resolve: (r: { run_id: string }) => resolve(r),
    reject: (e: unknown) => reject(e),
  };
}

beforeEach(() => {
  runsApi.createRun.mockReset();
  navigate.mockReset();
  useLaunchesInFlight.setState({ byTarget: {} });
  useUIStore.setState({ toasts: [] });
  authBox.value = null;
});
afterEach(cleanup);

describe("a Test launch whose pane was closed before its answer", () => {
  it("names the run it started, with the way to it", async () => {
    const run = pendingRun();
    const shown = render(pane());
    fireEvent.click(button());
    expect(button().textContent).toBe("Starting…");
    shown.unmount();
    await act(async () => run.resolve({ run_id: "run-1" }));

    expect(toasts().map((t) => t.message)).toEqual(["Run run-1 started for the bot nestor."]);
    expect(toasts()[0]).toMatchObject({ type: "success", persistent: true });
    toasts()[0]?.action?.onClick();
    expect(navigate).toHaveBeenCalledWith("/runs/run-1");
    expect(inFlight()).toEqual({});
  });

  it("says its refusal", async () => {
    const run = pendingRun();
    const shown = render(pane());
    fireEvent.click(button());
    shown.unmount();
    await act(async () =>
      run.reject(apiErrorFrom(402, { message: "monthly run quota exceeded", errorCode: "monthly_run_quota_exceeded" })),
    );
    expect(toasts()).toHaveLength(1);
    expect(toasts()[0]?.message).toMatch(/^The launch of the bot nestor was refused: .*monthly run quota exceeded/);
    expect(inFlight()).toEqual({});
  });

  it("is still held when the pane is opened again, and starts no second run", async () => {
    const run = pendingRun();
    const first = render(pane());
    fireEvent.click(button());
    first.unmount();

    render(pane());
    expect(button().textContent).toBe("Starting…");
    expect(button().disabled).toBe(true);
    fireEvent.click(button());
    expect(runsApi.createRun).toHaveBeenCalledTimes(1);

    await act(async () => run.resolve({ run_id: "run-1" }));
    expect(button().textContent).toBe("Test");
    expect(button().disabled).toBe(false);
    expect(toasts().map((t) => t.message)).toEqual(["Run run-1 started for the bot nestor."]);
  });
});

describe("the Test pane and the launch form of one bot", () => {
  it("hold each other: one key for the bot, whichever surface launches it", () => {
    // The form's launch of this bot is in flight (useLaunchSubmit holds the
    // same key): the pane is held.
    useLaunchesInFlight.getState().begin(botLaunchKey("", "nestor"));
    render(pane());
    expect(button().disabled).toBe(true);
    cleanup();

    // And the pane's own launch holds exactly that key, which the form reads.
    useLaunchesInFlight.setState({ byTarget: {} });
    pendingRun();
    render(pane());
    fireEvent.click(button());
    expect(inFlight()).toEqual({ [botLaunchKey("", "nestor")]: 1 });
  });

  // The bot is the team's: the key both surfaces hold carries the team the
  // session is scoped to, so the same slug in another team is not held.
  it("hold each other within a team, and not across two", () => {
    authBox.value = { activeTeamID: "team-1" };
    useLaunchesInFlight.getState().begin(botLaunchKey("team-2", "nestor"));
    render(pane());
    expect(button().disabled).toBe(false);
    cleanup();

    useLaunchesInFlight.setState({ byTarget: {} });
    useLaunchesInFlight.getState().begin(botLaunchKey("team-1", "nestor"));
    render(pane());
    expect(button().disabled).toBe(true);
    cleanup();

    useLaunchesInFlight.setState({ byTarget: {} });
    pendingRun();
    render(pane());
    fireEvent.click(button());
    expect(inFlight()).toEqual({ [botLaunchKey("team-1", "nestor")]: 1 });
  });
});

// On screen as in a notice (rva round 5, F2): a launch that drew no answer is
// not a plain failure. The button is free again, and the pane says what a
// second click risks, with the way to look first.
describe("a Test launch that drew no answer, with its pane still open", () => {
  for (const [name, error] of [
    ["a gateway timeout", apiErrorFrom(504, { message: "Gateway Timeout" })],
    ["a network failure", new TypeError("Failed to fetch")],
  ] as const) {
    it(`says the run may have started after ${name}, and where to look`, async () => {
      const run = pendingRun();
      render(pane());
      fireEvent.click(button());
      await act(async () => run.reject(error));

      const banner = screen.getByText(/No answer came for the launch of the bot nestor/);
      expect(banner.textContent).toMatch(/the run may have started — check its runs before launching again\./);
      expect(screen.getByRole("link", { name: "Open its runs" }).getAttribute("href")).toBe("/runs?bot=nestor");
      expect(button().textContent).toBe("Test");
      expect(button().disabled).toBe(false);
      expect(toasts()).toHaveLength(0);
    });
  }

  it("offers no such look for a refusal", async () => {
    const run = pendingRun();
    render(pane());
    fireEvent.click(button());
    await act(async () =>
      run.reject(apiErrorFrom(402, { message: "monthly run quota exceeded", errorCode: "monthly_run_quota_exceeded" })),
    );
    expect(screen.getByText(/monthly run quota exceeded/)).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Open its runs" })).toBeNull();
    expect(screen.queryByText(/may have started/)).toBeNull();
  });
});
