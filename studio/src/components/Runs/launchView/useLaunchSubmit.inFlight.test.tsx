// @vitest-environment jsdom
//
// A launch is sent from one place (lib/sendLaunch), and its answer is never
// lost (#3416, rva rounds 3 and 4): the target is held from before the first
// step of the launch until its answer, under the key of what the SERVER
// resolved; an outcome whose form is no longer on screen is announced for
// what it is — a run that started, a refusal, a launch that did not start,
// or no answer at all — and is never applied to another form.
//
// Asserted on useLaunchSubmit, wired as LaunchView wires it, with a
// repository to create first: the leg that used to run before the target was
// held, and whose failure was said to a form that had gone.
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const runsApi = vi.hoisted(() => ({ createRun: vi.fn() }));
vi.mock("@/api/runs", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/runs")>()),
  createRun: runsApi.createRun,
}));
const forgeApi = vi.hoisted(() => ({ createForgeRepo: vi.fn() }));
vi.mock("@/api/forgeConnections", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/forgeConnections")>()),
  createForgeRepo: forgeApi.createForgeRepo,
}));
const navigate = vi.hoisted(() => vi.fn());
vi.mock("wouter", async (importOriginal) => ({
  ...(await importOriginal<typeof import("wouter")>()),
  useLocation: () => ["/runs/new", navigate],
}));

import { apiErrorFrom, publishExplicitTeam, SessionRenewalUnavailableError } from "@/api/client";
import type { ServerInfo } from "@/api/types";
import { createEmptyDocument } from "@/lib/defaults";
import { botLaunchKey, forgetLaunches } from "@/lib/sendLaunch";
import { useLaunchesInFlight } from "@/store/launchesInFlight";
import { useUIStore } from "@/store/ui";

import { emptyBudgetFieldValues } from "./budgetPayload";
import { useLaunchSubmit } from "./useLaunchSubmit";
import type { UseRepoTargetResult } from "./useRepoTarget";
import type { UseRunOverridesResult } from "./useRunOverrides";

const noOverrides = {
  modelOverrides: {},
  budgetFields: emptyBudgetFieldValues,
} as unknown as UseRunOverridesResult;
const cloudInfo: ServerInfo = {
  mode: "cloud",
  version: "test",
  work_dir: "",
  limits: { upload: { max_file_size: 1, max_total_size: 1, max_files_per_run: 1, allowed_mime: [] } },
};
const doc = (() => {
  const d = createEmptyDocument();
  d.workflows = [
    { name: "w", entry: "done", nodes: [], edges: [], sandbox: { mode: "auto" } } as never,
  ];
  return d;
})();

// What the form set inline in its repository section, in order.
const createErrors: Array<string | null> = [];
// The form of one bot, as the launch-source read named it, with or without a
// repository to create before the launch.
function Form({ bot, createsRepo = false }: { bot: string; createsRepo?: boolean }) {
  const [error, setError] = useState<string | null>(null);
  const repoTarget = (
    createsRepo
      ? {
          activeRepo: null,
          teamRepos: [],
          teamID: "team-1",
          repoRequirement: { mode: "required" },
          showRepoTarget: true,
          repoTargetState: {
            mode: "create",
            createConnectionID: "conn-1",
            createOwner: "",
            createName: "new-repo",
            createPrivate: true,
          },
          setRepoTargetCreateError: (e: string | null) => createErrors.push(e),
          missingRepoTarget: false,
        }
      : {
          activeRepo: null,
          teamRepos: [],
          teamID: null,
          repoRequirement: null,
          showRepoTarget: false,
          repoTargetState: null,
          setRepoTargetCreateError: (e: string | null) => createErrors.push(e),
          missingRepoTarget: false,
        }
  ) as unknown as UseRepoTargetResult;
  const { onSubmit, submitting } = useLaunchSubmit({
    filePath: "",
    doc,
    currentSource: "x",
    namedBot: { id: bot, sha256: "sha" },
    confirmedDiskPath: null,
    serverInfo: cloudInfo,
    fields: [],
    values: {},
    attachmentFields: [],
    attachments: {},
    selectedPreset: "",
    selectedPresetMeta: undefined,
    repoTarget,
    overrides: noOverrides,
    setError,
  });
  return (
    <>
      <button onClick={onSubmit} disabled={submitting}>
        Launch
      </button>
      <output aria-label="error">{error ?? ""}</output>
    </>
  );
}

// A call whose answer the test gives later.
function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

const launch = () => screen.getByRole("button", { name: "Launch" }) as HTMLButtonElement;
const inFlight = () => useLaunchesInFlight.getState().byTarget;
const toasts = () => useUIStore.getState().toasts;
const formError = () => screen.getByLabelText("error").textContent;
const setErrors = () => createErrors.filter((e) => e !== null);

beforeEach(() => {
  createErrors.length = 0;
  runsApi.createRun.mockReset();
  forgeApi.createForgeRepo.mockReset();
  navigate.mockReset();
  useUIStore.setState({ toasts: [] });
  useLaunchesInFlight.setState({ byTarget: {} });
});
afterEach(() => {
  cleanup();
  publishExplicitTeam(null);
});

it("does not send a prepared launch after leaving its request team", async () => {
  publishExplicitTeam("team-a");
  const repo = deferred<{ clone_url: string }>();
  forgeApi.createForgeRepo.mockReturnValueOnce(repo.promise);
  const view = render(<Form bot="alpha" createsRepo />);
  fireEvent.click(launch());
  expect(forgeApi.createForgeRepo).toHaveBeenCalledTimes(1);

  publishExplicitTeam("team-b");
  view.rerender(<Form key="team-b" bot="beta" />);
  await act(async () => repo.resolve({ clone_url: "https://fixture.invalid/repo" }));

  expect(runsApi.createRun).not.toHaveBeenCalled();
  expect(inFlight()).toEqual({});
});

it("does not show the previous team's unanswered launch in this team", async () => {
  publishExplicitTeam("team-a");
  const run = deferred<{ run_id: string }>();
  runsApi.createRun.mockReturnValueOnce(run.promise);
  const view = render(<Form bot="alpha" />);
  fireEvent.click(launch());
  expect(runsApi.createRun).toHaveBeenCalledTimes(1);

  publishExplicitTeam("team-b");
  view.rerender(<Form key="team-b" bot="alpha" />);
  await act(async () => run.reject(apiErrorFrom(504, { message: "Gateway Timeout" })));

  expect(toasts()).toEqual([]);
  expect(formError()).toBe("");
});

describe("the target is held from the first step of its launch to its answer", () => {
  it("is held while its repository is being created, before any run is asked for", async () => {
    const repo = deferred<{ clone_url: string }>();
    forgeApi.createForgeRepo.mockReturnValueOnce(repo.promise);
    runsApi.createRun.mockResolvedValue({ run_id: "run-1" });
    render(<Form bot="alpha" createsRepo />);
    fireEvent.click(launch());

    expect(forgeApi.createForgeRepo).toHaveBeenCalledTimes(1);
    expect(runsApi.createRun).not.toHaveBeenCalled();
    expect(inFlight()).toEqual({ [botLaunchKey("", "alpha")]: 1 });
    expect(launch().disabled).toBe(true);

    await act(async () => repo.resolve({ clone_url: "https://forge/new-repo.git" }));
    expect(runsApi.createRun.mock.calls[0]?.[0]).toMatchObject({
      bot_id: "alpha",
      repo_url: "https://forge/new-repo.git",
      connection_id: "conn-1",
    });
    expect(inFlight()).toEqual({});
  });

  it("is freed by a run that started, on screen", async () => {
    runsApi.createRun.mockResolvedValue({ run_id: "run-1" });
    render(<Form bot="alpha" />);
    await act(async () => fireEvent.click(launch()));
    expect(navigate).toHaveBeenCalledWith("/runs/run-1");
    expect(inFlight()).toEqual({});
    expect(launch().disabled).toBe(false);
    expect(toasts()).toHaveLength(0);
  });

  it("is freed by a run that started, off screen", async () => {
    const run = deferred<{ run_id: string }>();
    runsApi.createRun.mockReturnValueOnce(run.promise);
    const r = render(<Form bot="alpha" />);
    fireEvent.click(launch());
    r.rerender(<Form bot="beta" />);
    await act(async () => run.resolve({ run_id: "run-1" }));
    expect(inFlight()).toEqual({});
    expect(navigate).not.toHaveBeenCalled();
    expect(toasts().map((t) => t.message)).toEqual(["Run run-1 started for the bot alpha."]);
    // Back on alpha, a new launch is the operator's to ask for.
    r.rerender(<Form bot="alpha" />);
    expect(launch().disabled).toBe(false);
  });
});

describe("a repository that could not be created, with its form gone", () => {
  const taken = () => new Error("409: repository name already taken");

  it("says the launch did not start when the form left the screen", async () => {
    const repo = deferred<{ clone_url: string }>();
    forgeApi.createForgeRepo.mockReturnValueOnce(repo.promise);
    const r = render(<Form bot="alpha" createsRepo />);
    fireEvent.click(launch());
    r.unmount();
    await act(async () => repo.reject(taken()));

    expect(toasts()).toHaveLength(1);
    expect(toasts()[0]).toMatchObject({ type: "error", persistent: true });
    expect(toasts()[0]?.message).toBe(
      "The launch of the bot alpha did not start: creating the repository failed — 409: repository name already taken",
    );
    expect(setErrors()).toEqual([]);
    expect(runsApi.createRun).not.toHaveBeenCalled();
    expect(inFlight()).toEqual({});
  });

  it("is not set on the form of the bot the operator moved to", async () => {
    const repo = deferred<{ clone_url: string }>();
    forgeApi.createForgeRepo.mockReturnValueOnce(repo.promise);
    const r = render(<Form bot="alpha" createsRepo />);
    fireEvent.click(launch());
    r.rerender(<Form bot="beta" createsRepo />);
    await act(async () => repo.reject(taken()));

    expect(setErrors()).toEqual([]);
    expect(toasts().map((t) => t.message)).toEqual([
      "The launch of the bot alpha did not start: creating the repository failed — 409: repository name already taken",
    ]);
    expect(launch().disabled).toBe(false);
  });

  it("is said inline, where it can be acted on, when the form is still there", async () => {
    forgeApi.createForgeRepo.mockRejectedValueOnce(taken());
    render(<Form bot="alpha" createsRepo />);
    await act(async () => fireEvent.click(launch()));
    expect(setErrors()).toEqual(["409: repository name already taken"]);
    expect(toasts()).toHaveLength(0);
    expect(inFlight()).toEqual({});
  });
});

// "Refused" is a verdict the server gave. A network failure or a gateway's
// 5xx gave none: the run may exist and be billed, and a notice that says
// "refused" invites a second one.
describe("an outcome nobody heard, with its form gone", () => {
  async function failOffScreen(error: unknown) {
    const run = deferred<{ run_id: string }>();
    runsApi.createRun.mockReturnValueOnce(run.promise);
    const r = render(<Form bot="alpha" />);
    fireEvent.click(launch());
    r.rerender(<Form bot="beta" />);
    await act(async () => run.reject(error));
    expect(toasts()).toHaveLength(1);
    return toasts()[0];
  }

  for (const [name, error] of [
    ["a network failure", new TypeError("Failed to fetch")],
    ["a gateway timeout", apiErrorFrom(504, { message: "Gateway Timeout" })],
  ] as const) {
    it(`says that no answer came after ${name}, and where to look before launching again`, async () => {
      const toast = await failOffScreen(error);
      expect(toast?.message).toMatch(
        /^No answer came for the launch of the bot alpha \(.+\): the run may have started — check its runs before launching again\.$/,
      );
      expect(toast?.message).not.toMatch(/refused/);
      expect(toast).toMatchObject({ type: "warning", persistent: true });
      expect(toast?.action?.label).toBe("Open its runs");
      toast?.action?.onClick();
      expect(navigate).toHaveBeenCalledWith("/runs?bot=alpha");
    });
  }

  // The POST was answered 401 and the session could not be renewed: the
  // server launched nothing, and "may have started" would be untrue.
  it("says did not start when the session could not be renewed", async () => {
    const toast = await failOffScreen(new SessionRenewalUnavailableError(503));
    expect(toast?.message).toMatch(/^The launch of the bot alpha did not start: .*the session could not be renewed/);
    expect(toast?.message).not.toMatch(/may have started/);
    expect(toast?.type).toBe("error");
    expect(toast?.action).toBeUndefined();
  });

  it("says refused for an answer that refused — a quota", async () => {
    const toast = await failOffScreen(
      apiErrorFrom(402, { message: "monthly run quota exceeded", errorCode: "monthly_run_quota_exceeded" }),
    );
    expect(toast?.message).toMatch(/^The launch of the bot alpha was refused: .*monthly run quota exceeded/);
    expect(toast?.type).toBe("error");
    expect(toast?.action).toBeUndefined();
    expect(formError()).toBe("");
  });
});

// Two launches in flight, their answers in either order: each outcome goes
// to its own target — the one on screen to its form, the other to a notice.
describe("two launches whose answers race", () => {
  for (const first of ["alpha", "beta"] as const) {
    it(`gives each its own, ${first} answering first`, async () => {
      const runs = { alpha: deferred<{ run_id: string }>(), beta: deferred<{ run_id: string }>() };
      runsApi.createRun.mockImplementation((req: { bot_id: "alpha" | "beta" }) => runs[req.bot_id].promise);
      const r = render(<Form bot="alpha" />);
      fireEvent.click(launch());
      r.rerender(<Form bot="beta" />);
      fireEvent.click(launch());
      expect(inFlight()).toEqual({ [botLaunchKey("", "alpha")]: 1, [botLaunchKey("", "beta")]: 1 });

      const second = first === "alpha" ? "beta" : "alpha";
      await act(async () => runs[first].resolve({ run_id: `run-${first}` }));
      await act(async () => runs[second].resolve({ run_id: `run-${second}` }));

      expect(inFlight()).toEqual({});
      // Beta's form is on screen: its run is where the operator is taken.
      expect(navigate.mock.calls).toEqual([["/runs/run-beta"]]);
      expect(toasts().map((t) => t.message)).toEqual(["Run run-alpha started for the bot alpha."]);
    });
  }
});

// The store outlives a form — and must not outlive an account.
describe("an account that changes while a launch is in flight", () => {
  it("is neither held nor told of the run of the account before it", async () => {
    const run = deferred<{ run_id: string }>();
    runsApi.createRun.mockReturnValueOnce(run.promise);
    useUIStore.getState().addToast("Saved.", "success", { persistent: true });
    useUIStore.getState().addToast("Run run-0 started for the bot zeta.", "success", {
      persistent: true,
      key: "launch:run-0",
    });
    render(<Form bot="alpha" />);
    fireEvent.click(launch());
    expect(launch().disabled).toBe(true);

    act(() => forgetLaunches());
    expect(inFlight()).toEqual({});
    expect(launch().disabled).toBe(false);
    // The launch notices go; what is not about a launch stays.
    expect(toasts().map((t) => t.message)).toEqual(["Saved."]);

    await act(async () => run.resolve({ run_id: "run-1" }));
    expect(toasts().map((t) => t.message)).toEqual(["Saved."]);
    expect(navigate).not.toHaveBeenCalled();
    expect(inFlight()).toEqual({});
  });

  // The next account launches the same target: its hold is its own, and the
  // answer of the account before it does not free it.
  it("does not free the next account's hold of the same target", async () => {
    const before = deferred<{ run_id: string }>();
    const after = deferred<{ run_id: string }>();
    runsApi.createRun.mockReturnValueOnce(before.promise).mockReturnValueOnce(after.promise);
    render(<Form bot="alpha" />);
    fireEvent.click(launch());
    act(() => forgetLaunches());

    fireEvent.click(launch());
    expect(runsApi.createRun).toHaveBeenCalledTimes(2);
    expect(inFlight()).toEqual({ [botLaunchKey("", "alpha")]: 1 });

    await act(async () => before.resolve({ run_id: "run-before" }));
    expect(inFlight()).toEqual({ [botLaunchKey("", "alpha")]: 1 });
    expect(launch().disabled).toBe(true);
    expect(navigate).not.toHaveBeenCalled();

    await act(async () => after.resolve({ run_id: "run-after" }));
    expect(inFlight()).toEqual({});
    expect(navigate.mock.calls).toEqual([["/runs/run-after"]]);
  });
});
