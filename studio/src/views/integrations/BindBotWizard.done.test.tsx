// @vitest-environment jsdom
// The bind wizard ends on "Launch now". It launches the bot just bound the
// way the session launches a bot (#3416): in cloud by name — a team's own bot
// has no workspace path, and the step offered it no launch at all.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import type { BotEntryWithSchema } from "@/api/bots";
import type { ForgeTeamRepo } from "@/api/forgeConnections";

const search = "step=done&repo=c1%3A%3Aorg%2Fapi&bot=nestor";
const navigate = vi.hoisted(() => vi.fn());

vi.mock("wouter", async () => {
  const actual = await vi.importActual<typeof import("wouter")>("wouter");
  return {
    ...actual,
    useSearch: () => search,
    useLocation: () => ["/integrations/bind", navigate],
  };
});

const session = vi.hoisted(() => ({ role: "admin" }));
vi.mock("@/auth/AuthContext", async () => {
  const actual = await vi.importActual<typeof import("@/auth/AuthContext")>("@/auth/AuthContext");
  const auth = () => ({
    activeTeam: { team_id: "t1", team_name: "Team" },
    activeTeamID: "t1",
    activeRole: session.role,
    user: { id: "u1", is_super_admin: false },
    orgs: [{ org_id: "o1", org_role: "member", teams: [{ team_id: "t1", role: session.role }] }],
  });
  return { ...actual, useAuth: auth, useMaybeAuth: auth };
});

vi.mock("@/api/forgeConnections", async () => {
  const actual =
    await vi.importActual<typeof import("@/api/forgeConnections")>("@/api/forgeConnections");
  return {
    ...actual,
    listTeamForgeRepos: vi.fn(async () => [
      {
        connection_id: "c1",
        provider: "gitlab",
        repo_full_name: "org/api",
        integration_id: "i1",
        bot_ids: ["nestor"],
        sync_issues_enabled: false,
      } as ForgeTeamRepo,
    ]),
  };
});
vi.mock("@/api/secrets", async () => {
  const actual = await vi.importActual<typeof import("@/api/secrets")>("@/api/secrets");
  return { ...actual, listTeamSecrets: vi.fn(async () => []) };
});

import { useBotsStore } from "@/store/bots";
import { useServerInfoStore } from "@/store/serverInfo";
import BindBotWizard from "@/views/integrations/BindBotWizard";

// A team's own bot: stored, so the registry gives it no workspace path.
const teamBot = {
  name: "nestor",
  display_name: "Nestor",
  path: "",
  origin: "tenant",
  is_bundle: true,
  invocations: [{ kind: "forge", forge: { event: "merge_request" } }],
} as unknown as BotEntryWithSchema;

const renderWizard = () =>
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <BindBotWizard />
    </QueryClientProvider>,
  );

beforeEach(() => {
  vi.clearAllMocks();
  session.role = "admin";
  useServerInfoStore.setState({ info: { mode: "cloud" } as never });
  useBotsStore.setState({ bots: [teamBot], loading: false, error: null });
});
// A control held for a reason stays focusable: it says it is unavailable
// (aria-disabled) and its description is the reason (HeldControl).
const held = (el: HTMLElement | null) =>
  !!el && ((el as HTMLButtonElement).disabled || el.getAttribute("aria-disabled") === "true");
const heldReason = (el: HTMLElement | null) =>
  el?.getAttribute("aria-disabled") === "true"
    ? (document.getElementById(el.getAttribute("aria-describedby") ?? "")?.textContent ?? "")
    : "";

afterEach(cleanup);

describe("BindBotWizard — the done step's Launch now", () => {
  it("opens the bound team bot's named launch form", async () => {
    renderWizard();
    const launch = (await screen.findByRole("button", { name: "Launch now Nestor" })) as HTMLButtonElement;
    expect(held(launch)).toBe(false);
    fireEvent.click(launch);
    expect(navigate).toHaveBeenCalledWith("/runs/new?bot=nestor");
  });

  it("is held for a member, who may not read the bot's program", async () => {
    session.role = "member";
    renderWizard();
    const launch = (await screen.findByRole("button", { name: "Launch now Nestor" })) as HTMLButtonElement;
    expect(launch.disabled).toBe(false);
    expect(heldReason(launch)).toContain("`iterion remote runs launch --bot nestor`");
  });
});
