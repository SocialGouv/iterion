// @vitest-environment jsdom
// A repository's page lists the bots bound to it, each with a Launch. That
// affordance launches the bot the way the session launches a bot (#3416): by
// name in cloud — a team's own bot has no workspace path, and was offered no
// Launch at all — and it is held, saying why, for a caller who may not read
// the bot's program.
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import type { BotEntryWithSchema } from "@/api/bots";
import type { ForgeTeamRepo } from "@/api/forgeConnections";

import { encodeRepoKey } from "./repoKey";

const navigate = vi.hoisted(() => vi.fn());
vi.mock("wouter", async () => {
  const actual = await vi.importActual<typeof import("wouter")>("wouter");
  return {
    ...actual,
    useParams: () => ({ key: encodeRepoKey("c1::org/api") }),
    useLocation: () => ["/repos/x", navigate],
  };
});

const repoRow = {
  connection_id: "c1",
  connection_status: "active",
  provider: "gitlab",
  repo_full_name: "org/api",
  integration_id: "i1",
  bot_ids: ["nestor"],
  sync_issues_enabled: false,
} as ForgeTeamRepo;

vi.mock("@/hooks/useActiveRepo", () => ({
  useActiveRepo: () => ({
    activeRepo: repoRow,
    overview: false,
    repos: [repoRow],
    loading: false,
    enabled: true,
    choose: vi.fn(),
    teamID: "t1",
  }),
}));
vi.mock("@/hooks/useCanManageTeam", () => ({ useCanManageTeam: () => false }));
vi.mock("@/api/forgeConnections", async () => {
  const actual =
    await vi.importActual<typeof import("@/api/forgeConnections")>("@/api/forgeConnections");
  return { ...actual, listForgeIntegrations: vi.fn(async () => []) };
});
vi.mock("@/api/schedules", async () => {
  const actual = await vi.importActual<typeof import("@/api/schedules")>("@/api/schedules");
  return { ...actual, listTeamSchedules: vi.fn(async () => []) };
});
// The session the launch decision reads: a member of the team, by default.
const session = vi.hoisted(() => ({ role: "member", superAdmin: false }));
vi.mock("@/auth/AuthContext", async () => {
  const actual = await vi.importActual<typeof import("@/auth/AuthContext")>("@/auth/AuthContext");
  return {
    ...actual,
    useMaybeAuth: () => ({
      activeTeamID: "t1",
      activeRole: session.role,
      user: { id: "u1", is_super_admin: session.superAdmin },
      orgs: [{ org_id: "o1", org_role: "member", teams: [{ team_id: "t1", role: session.role }] }],
    }),
  };
});

import { useBotsStore } from "@/store/bots";
import { useServerInfoStore } from "@/store/serverInfo";

import RepoDetailView from "./index";

// A team's own bot: stored, so the registry gives it no workspace path.
const teamBot: BotEntryWithSchema = { name: "nestor", path: "", origin: "tenant", is_bundle: true };
const bakedBot: BotEntryWithSchema = {
  name: "nestor",
  path: "/app/bots/nestor",
  rel_path: "bots/nestor",
  is_bundle: true,
  origin: "catalog",
};

function renderPage(mode: "cloud" | "local", bot: BotEntryWithSchema) {
  useServerInfoStore.setState({ info: { mode } as never });
  useBotsStore.setState({ bots: [bot], loading: false, error: null });
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <RepoDetailView />
    </QueryClientProvider>,
  );
}

// A control held for a reason stays focusable: it says it is unavailable
// (aria-disabled) and its description is the reason (HeldControl).
const held = (el: HTMLElement | null) =>
  !!el && ((el as HTMLButtonElement).disabled || el.getAttribute("aria-disabled") === "true");
const heldReason = (el: HTMLElement | null) =>
  el?.getAttribute("aria-disabled") === "true"
    ? (document.getElementById(el.getAttribute("aria-describedby") ?? "")?.textContent ?? "")
    : "";
// The row action names its row: "Launch" alone would be every row's name.
const launch = () =>
  screen.queryByRole("button", { name: "Launch nestor" }) as HTMLButtonElement | null;

beforeEach(() => {
  vi.clearAllMocks();
  session.role = "member";
  session.superAdmin = false;
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

describe("RepoDetail — a bound bot's Launch", () => {
  it("opens a team bot's named launch form in cloud, for one who may read it", () => {
    session.role = "admin";
    renderPage("cloud", teamBot);
    expect(held(launch())).toBe(false);
    fireEvent.click(launch()!);
    expect(navigate).toHaveBeenCalledWith("/runs/new?bot=nestor");
  });

  it("opens a baked bot's form by name too in cloud, never by its path", () => {
    renderPage("cloud", bakedBot);
    fireEvent.click(launch()!);
    expect(navigate).toHaveBeenCalledWith("/runs/new?bot=nestor");
  });

  it("is held for a member on a team bot, saying why and how to launch it", () => {
    renderPage("cloud", teamBot);
    expect(launch()?.disabled).toBe(false);
    expect(heldReason(launch())).toMatch(/a team's own bot is readable by a bot editor, a team admin or an owner/);
    expect(heldReason(launch())).toContain("`iterion remote runs launch --bot nestor`");
    fireEvent.click(launch()!);
    expect(navigate).not.toHaveBeenCalled();
  });

  it("leaves the focus on Launch when the hold lifts", () => {
    session.role = "member";
    renderPage("cloud", teamBot);
    const waitingOn = launch() as HTMLButtonElement;
    expect(held(waitingOn)).toBe(true);
    waitingOn.focus();
    session.role = "admin";
    act(() => useBotsStore.setState({ bots: [{ ...teamBot }] }));
    expect(held(launch())).toBe(false);
    expect(launch()).toBe(waitingOn);
    expect(document.activeElement).toBe(waitingOn);
  });

  it("opens a workspace bot's form by its file locally, and offers none without a path", () => {
    renderPage("local", bakedBot);
    fireEvent.click(launch()!);
    expect(navigate).toHaveBeenCalledWith("/runs/new?file=bots%2Fnestor%2Fmain.bot");
    cleanup();
    renderPage("local", teamBot);
    expect(launch()).toBeNull();
  });
});
