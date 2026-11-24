// @vitest-environment jsdom
//
// A bot's page offers its launch wherever the bot can be launched (#3416).
// In cloud every bot the registry lists is launchable BY NAME — a stored bot
// (a team's own, a platform override) has no workspace path, and "Launch"
// was dead for exactly those. Locally the launch still reads the bot's
// workspace file, and a bot the server could not relativise stays gated.
// One helper decides (botLaunch); this page's Launch button and its Test
// pane both follow it.
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { Route, Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";

const runsApi = vi.hoisted(() => ({ createRun: vi.fn() }));
vi.mock("@/api/runs", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/runs")>()),
  createRun: runsApi.createRun,
}));
// The session, as both readers of it see it: the actions row (useAuth) and
// the launch decision (useMaybeAuth). A team admin unless a test says otherwise.
const session = vi.hoisted(() => ({
  activeRole: "admin" as string | null,
  activeOrgRole: "member" as string | null,
  // The identity tree's picture of the team membership, when it differs from
  // the credential's (activeRole); undefined = the same.
  treeRole: undefined as string | undefined,
  // The org document did not load: the tree names the org and no team.
  orgUnavailable: false,
  // The active team, which the Test pane's target carries.
  teamID: "team-1",
  user: { id: "u1", is_super_admin: false },
}));
vi.mock("@/auth/AuthContext", async (importOriginal) => {
  // The identity as the app holds it: the active team's role, and the org
  // tree the manage-team rule reads (useCanManageTeam).
  const auth = () => ({
    activeTeamID: session.teamID,
    activeRole: session.activeRole,
    user: session.user,
    orgs: session.orgUnavailable
      ? [{ org_id: "org-1", unavailable: true, teams: [] }]
      : [
          {
            org_id: "org-1",
            org_role: session.activeOrgRole,
            teams: [{ team_id: "team-1", role: session.treeRole ?? session.activeRole }],
          },
        ],
  });
  return {
    ...(await importOriginal<typeof import("@/auth/AuthContext")>()),
    useAuth: auth,
    useMaybeAuth: auth,
  };
});
vi.mock("@/hooks/useActiveRepo", () => ({
  useActiveRepo: () => ({
    activeRepo: null,
    overview: false,
    repos: [],
    loading: false,
    enabled: false,
    choose: () => {},
    teamID: null,
  }),
}));
// The page's other cards read their own endpoints; this file is about the
// actions row and the test pane.
vi.mock("./AutomationsCard", () => ({ default: () => null }));
vi.mock("./ConfigSharesCard", () => ({ ConfigSharesCard: () => null }));
vi.mock("./IdentityHeader", () => ({ default: () => null }));
vi.mock("./MetadataCard", () => ({ default: () => null }));
vi.mock("./PresetsCard", () => ({ default: () => null }));
vi.mock("./RecentRunsCard", () => ({ default: () => null }));
vi.mock("./SecretBindingsCard", () => ({ default: () => null }));
vi.mock("./VarsCard", () => ({ default: () => null }));

import type { BotEntryWithSchema } from "@/api/bots";
import type { ServerInfo } from "@/api/types";
import { useBotsStore } from "@/store/bots";
import { botLaunchKey } from "@/lib/sendLaunch";
import { useLaunchesInFlight } from "@/store/launchesInFlight";
import { useServerInfoStore } from "@/store/serverInfo";
import { useUIStore } from "@/store/ui";

import BotHomeView from "./index";

const limits = {
  upload: { max_file_size: 1, max_total_size: 1, max_files_per_run: 1, allowed_mime: [] },
};
const cloudInfo: ServerInfo = { mode: "cloud", version: "test", work_dir: "", limits };
const localInfo: ServerInfo = { mode: "local", version: "test", work_dir: "/w", limits };

// A stored bot: the registry gives it no workspace path.
const storedBot: BotEntryWithSchema = {
  name: "nestor",
  path: "",
  origin: "tenant",
  editable: true,
  is_bundle: true,
};
const workspaceBot: BotEntryWithSchema = {
  name: "nestor",
  path: "/w/bots/nestor",
  rel_path: "bots/nestor",
  is_bundle: true,
  origin: "catalog",
};

function mount(info: ServerInfo, bot: BotEntryWithSchema) {
  useServerInfoStore.setState({ info });
  useBotsStore.setState({ bots: [bot], loading: false, error: null });
  const loc = memoryLocation({ path: "/bots/nestor", record: true });
  render(
    <Router hook={loc.hook}>
      <Route path="/bots/:name">
        <BotHomeView />
      </Route>
    </Router>,
  );
  return loc;
}

const launch = () => screen.getByRole("button", { name: "Launch" }) as HTMLButtonElement;
const test = () => screen.getByRole("button", { name: "Test" }) as HTMLButtonElement;
// A control held for a reason stays focusable: it says it is unavailable
// (aria-disabled) and its description is the reason (HeldControl).
const held = (el: HTMLElement | null) =>
  !!el && ((el as HTMLButtonElement).disabled || el.getAttribute("aria-disabled") === "true");
const heldReason = (el: HTMLElement | null) =>
  el?.getAttribute("aria-disabled") === "true"
    ? (document.getElementById(el.getAttribute("aria-describedby") ?? "")?.textContent ?? "")
    : "";
// Opens the test pane and presses its own start button: once the pane is
// open the page's toggle reads "Close test", so "Test" names the pane's.
function startTest() {
  fireEvent.click(test());
  fireEvent.click(within(screen.getByRole("region", { name: "Test run" })).getByRole("button", { name: "Test" }));
}

beforeEach(() => {
  vi.resetAllMocks();
  // The launch is observed at the request; the run view is not this file's.
  runsApi.createRun.mockReturnValue(new Promise(() => {}));
  session.activeRole = "admin";
  session.activeOrgRole = "member";
  session.treeRole = undefined;
  session.orgUnavailable = false;
  session.teamID = "team-1";
  useUIStore.setState({ toasts: [] });
  // A launch a test left in flight is that test's.
  useLaunchesInFlight.setState({ byTarget: {} });
  session.user = { id: "u1", is_super_admin: false };
});

afterEach(cleanup);

// A held control that takes the focus opens its tooltip, and Radix positioning
// (floating-ui) needs ResizeObserver, which jsdom lacks.
beforeAll(() => {
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
});

describe("a stored bot's page in cloud", () => {
  it("offers Launch, which opens the bot's named launch form", () => {
    const loc = mount(cloudInfo, storedBot);
    expect(launch().disabled).toBe(false);
    fireEvent.click(launch());
    expect(loc.history).toContain("/runs/new?bot=nestor");
  });

  it("offers Test, which launches the bot by its id", async () => {
    mount(cloudInfo, storedBot);
    expect(test().disabled).toBe(false);
    startTest();
    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(1));
    const posted = runsApi.createRun.mock.calls[0]?.[0] as Record<string, unknown>;
    expect(posted).toMatchObject({ bot_id: "nestor", merge_into: "none" });
    expect(posted).not.toHaveProperty("file_path");
    expect(posted).not.toHaveProperty("source");
  });
});

describe("a bot's page locally", () => {
  it("keeps Launch and Test gated for a bot with no workspace path", () => {
    const loc = mount(localInfo, storedBot);
    expect(launch().disabled).toBe(true);
    expect(launch().title).toMatch(/couldn't relativise this bot's path/);
    expect(test().disabled).toBe(true);
    fireEvent.click(launch());
    expect(loc.history).toEqual(["/bots/nestor"]);
  });

  it("opens a workspace bot's launch form by its slug, as before", () => {
    const loc = mount(localInfo, workspaceBot);
    expect(launch().disabled).toBe(false);
    fireEvent.click(launch());
    expect(loc.history).toContain("/runs/new?bot=nestor");
  });

  it("tests a workspace bot by its file, as before", async () => {
    mount(localInfo, workspaceBot);
    startTest();
    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(1));
    const posted = runsApi.createRun.mock.calls[0]?.[0] as Record<string, unknown>;
    expect(posted).toMatchObject({ file_path: "bots/nestor/main.bot", merge_into: "none" });
    expect(posted).not.toHaveProperty("bot_id");
  });
});

// "Launch" opens the form, and the form shows the bot's program: in cloud it
// is offered to those the server serves that program to, and to nobody it
// would answer 403 — a dead end with nothing to do on it (rva round 1, S3).
// "Test" launches by id and shows no program, so it stays theirs.
describe("a bot's page in cloud, for a caller who may not read its program", () => {
  const platformBot: BotEntryWithSchema = { ...storedBot, origin: "platform", editable: false };

  it("holds Launch on a team's own bot for a member, and says how to launch it", () => {
    session.activeRole = "member";
    const loc = mount(cloudInfo, storedBot);
    // Held where a keyboard reaches the reason: focusable, not disabled.
    expect(launch().disabled).toBe(false);
    expect(heldReason(launch())).toMatch(/a team's own bot is readable by a bot editor, a team admin or an owner/);
    expect(heldReason(launch())).toContain("`iterion remote runs launch --bot nestor`");
    fireEvent.click(launch());
    expect(loc.history).toEqual(["/bots/nestor"]);
  });

  it("still offers Test to that member, which launches the bot by its id", async () => {
    session.activeRole = "member";
    mount(cloudInfo, storedBot);
    expect(test().disabled).toBe(false);
    startTest();
    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(1));
    expect(runsApi.createRun.mock.calls[0]?.[0]).toMatchObject({ bot_id: "nestor" });
  });

  it("offers Launch on a team's own bot to a bot editor and to an admin of its org", () => {
    session.activeRole = "config_editor";
    mount(cloudInfo, storedBot);
    expect(held(launch())).toBe(false);
    cleanup();
    session.activeRole = "member";
    session.activeOrgRole = "admin";
    mount(cloudInfo, storedBot);
    expect(held(launch())).toBe(false);
  });

  // The server reads the membership row; the session holds two pictures of
  // it, and either can lag (rva round 2, S3e). Neither lag may hold Launch
  // for a caller the server serves.
  it("offers Launch to a team owner whose org document did not load", () => {
    session.activeRole = "owner";
    session.orgUnavailable = true;
    mount(cloudInfo, storedBot);
    expect(held(launch())).toBe(false);
  });

  it("offers Launch to a bot editor granted since their credential was issued", () => {
    session.activeRole = "member";
    session.treeRole = "config_editor";
    mount(cloudInfo, storedBot);
    expect(held(launch())).toBe(false);
  });

  it("leaves the focus on Launch when the hold lifts", () => {
    session.activeRole = "member";
    mount(cloudInfo, storedBot);
    const waitingOn = launch();
    expect(held(waitingOn)).toBe(true);
    waitingOn.focus();
    // The role is raised, and the page renders again.
    session.activeRole = "admin";
    act(() => useBotsStore.setState({ bots: [{ ...storedBot }] }));
    expect(held(launch())).toBe(false);
    expect(launch()).toBe(waitingOn);
    expect(document.activeElement).toBe(waitingOn);
  });

  it("still holds Launch for one who is a member in both pictures, org document or not", () => {
    session.activeRole = "member";
    session.orgUnavailable = true;
    mount(cloudInfo, storedBot);
    expect(heldReason(launch())).toMatch(/a team's own bot is readable by a bot editor/);
  });

  it("offers Launch on a platform override to a member, as on a baked bot", () => {
    session.activeRole = "member";
    const loc = mount(cloudInfo, platformBot);
    expect(held(launch())).toBe(false);
    fireEvent.click(launch());
    expect(loc.history).toContain("/runs/new?bot=nestor");
  });

  it("offers Launch on a baked bot to a member", () => {
    session.activeRole = "member";
    mount(cloudInfo, { ...workspaceBot });
    expect(held(launch())).toBe(false);
  });
});

// The page stays mounted from one bot to the next (/bots/:name), and the Test
// pane with it: everything the pane holds is ONE bot's — in one team — so it
// is another pane for another target (rva round 5, F1). A pane kept across
// the move showed alpha's run under beta, left beta's Test free while
// alpha's was pending, and posted alpha's untouched value in beta's run.
describe("a bot's Test pane is that bot's, in that team", () => {
  const str = (v: string) => ({ kind: "string" as const, str_val: v, raw: JSON.stringify(v) });
  const botWith = (name: string, shared: string): BotEntryWithSchema => ({
    ...storedBot,
    name,
    vars: { fields: [{ name: "shared", type: "string", default: str(shared) }] },
  });
  const alpha = botWith("alpha", "alpha-default");
  const beta = botWith("beta", "beta-default");
  function mountBots(path: string) {
    useServerInfoStore.setState({ info: cloudInfo });
    useBotsStore.setState({ bots: [alpha, beta], loading: false, error: null });
    const loc = memoryLocation({ path, record: true });
    render(
      <Router hook={loc.hook}>
        <Route path="/bots/:name">
          <BotHomeView />
        </Route>
      </Router>,
    );
    return loc;
  }
  const pane = () => within(screen.getByRole("region", { name: "Test run" }));
  const paneButton = () => pane().getByRole("button", { name: /^(Test|Starting…)$/ }) as HTMLButtonElement;
  const sharedField = () => pane().getByLabelText("Var shared") as HTMLInputElement;
  const toasts = () => useUIStore.getState().toasts.map((t) => t.message);
  function pendingRun() {
    let resolve!: (r: { run_id: string }) => void;
    runsApi.createRun.mockImplementationOnce(() => new Promise((res) => (resolve = res)));
    return (run_id: string) => act(async () => resolve({ run_id }));
  }

  it("leaves the next bot's Test free, and names the first bot's run instead of showing it there", async () => {
    const answer = pendingRun();
    const loc = mountBots("/bots/alpha");
    startTest();
    expect(paneButton().textContent).toBe("Starting…");
    expect(useLaunchesInFlight.getState().byTarget).toEqual({ [botLaunchKey("team-1", "alpha")]: 1 });

    act(() => loc.navigate("/bots/beta"));
    expect(paneButton().textContent).toBe("Test");
    expect(paneButton().disabled).toBe(false);

    await answer("run-A");
    expect(toasts()).toEqual(["Run run-A started for the bot alpha."]);
    // Beta's pane is still beta's: no run of alpha in it.
    expect(paneButton().textContent).toBe("Test");
    expect(useLaunchesInFlight.getState().byTarget).toEqual({});
  });

  it("starts from the next bot's own values, and posts none of the first bot's", async () => {
    const loc = mountBots("/bots/alpha");
    fireEvent.click(test());
    expect(sharedField().value).toBe("alpha-default");

    act(() => loc.navigate("/bots/beta"));
    expect(sharedField().value).toBe("beta-default");
    fireEvent.click(paneButton());
    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(1));
    const posted = runsApi.createRun.mock.calls[0]?.[0] as Record<string, unknown>;
    expect(posted).toMatchObject({ bot_id: "beta" });
    expect(posted).not.toHaveProperty("vars");
  });

  it("is another pane under another team: the first team's run is named, not shown", async () => {
    const answer = pendingRun();
    mountBots("/bots/alpha");
    startTest();
    expect(paneButton().textContent).toBe("Starting…");

    // The session moves to a team that has a bot of that name.
    session.teamID = "team-2";
    act(() => useBotsStore.setState({ bots: [{ ...alpha }, beta] }));
    expect(paneButton().textContent).toBe("Test");
    expect(paneButton().disabled).toBe(false);

    await answer("run-team1");
    expect(toasts()).toEqual(["Run run-team1 started for the bot alpha."]);
    expect(paneButton().textContent).toBe("Test");
  });
});
