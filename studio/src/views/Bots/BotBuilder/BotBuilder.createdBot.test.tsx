// @vitest-environment jsdom
// The builder ends on a test pane for the bot it just created. In cloud that
// bot lives in the team's store and has no workspace path, so it is tested
// the way the session launches a bot (#3416): by its id — the pane was
// "unavailable" for exactly the bots the cloud builder creates.
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const runsApi = vi.hoisted(() => ({ createRun: vi.fn() }));
vi.mock("@/api/runs", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/runs")>()),
  createRun: runsApi.createRun,
}));
const team = vi.hoisted(() => ({ id: "team-1" }));
vi.mock("@/auth/AuthContext", async (importOriginal) => {
  const auth = () => ({
    activeTeamID: team.id,
    activeRole: "admin",
    user: { id: "u1", is_super_admin: false },
    orgs: [{ org_id: "o1", org_role: "member", teams: [{ team_id: team.id, role: "admin" }] }],
  });
  return {
    ...(await importOriginal<typeof import("@/auth/AuthContext")>()),
    useAuth: auth,
    useMaybeAuth: auth,
  };
});
// The form's cards are not this file's: it is about what follows a create.
vi.mock("./PrimaryFieldsCard", () => ({ default: () => null }));
vi.mock("./ModelSkillsCard", () => ({ default: () => null }));
vi.mock("./VarsEditorCard", () => ({ default: () => null }));
vi.mock("./AdvancedCard", () => ({ default: () => null }));
vi.mock("./SummaryCard", () => ({ default: () => null }));
vi.mock("./TemplateGallery", () => ({ default: () => null }));
vi.mock("./CreatedBotBanner", () => ({ default: () => null }));

const botsApi = vi.hoisted(() => ({ createBot: vi.fn(), listBots: vi.fn() }));
vi.mock("@/api/bots", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/bots")>()),
  ...botsApi,
}));
import { DRAFT_KEY, emptyDraft } from "./model";

import type { BotEntryWithSchema } from "@/api/bots";
import { useServerInfoStore } from "@/store/serverInfo";

import BotBuilderView from "./index";

// What the cloud create answers: a team bot, stored, with no workspace path.
const storedBot: BotEntryWithSchema = { name: "nestor", path: "", origin: "tenant", is_bundle: true };
const workspaceBot: BotEntryWithSchema = {
  name: "nestor",
  path: "/w/bots/nestor",
  rel_path: "bots/nestor",
  is_bundle: true,
};

async function mount(mode: "cloud" | "local", entry: BotEntryWithSchema) {
  localStorage.setItem(DRAFT_KEY, JSON.stringify({ ...emptyDraft(), phase: 2, name: "Nestor", instructions: "watch" }));
  botsApi.createBot.mockResolvedValueOnce(entry);
  useServerInfoStore.setState({ info: { mode, bot_editing_enabled: true } as never });
  const view = render(<BotBuilderView />);
  fireEvent.click(screen.getByRole("button", { name: /create bot/i }));
  await screen.findByRole("heading", { name: "Bot created" });
  return view;
}

const startTest = () =>
  fireEvent.click(
    within(screen.getByRole("region", { name: "Test run" })).getByRole("button", { name: "Test" }),
  );

beforeEach(() => {
  vi.resetAllMocks();
  localStorage.removeItem(DRAFT_KEY);
  team.id = "team-1";
  botsApi.listBots.mockResolvedValue([]);
  runsApi.createRun.mockReturnValue(new Promise(() => {}));
});
afterEach(cleanup);

describe("BotBuilder — the test pane of the bot just created", () => {
  it("tests a cloud bot by its id", async () => {
    await mount("cloud", storedBot);
    startTest();
    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(1));
    const posted = runsApi.createRun.mock.calls[0]?.[0] as Record<string, unknown>;
    expect(posted).toMatchObject({ bot_id: "nestor", merge_into: "none" });
    expect(posted).not.toHaveProperty("file_path");
  });

  it("tests a local bot by its workspace file, as before", async () => {
    await mount("local", workspaceBot);
    startTest();
    await waitFor(() => expect(runsApi.createRun).toHaveBeenCalledTimes(1));
    expect(runsApi.createRun.mock.calls[0]?.[0]).toMatchObject({
      file_path: "bots/nestor/main.bot",
      merge_into: "none",
    });
  });

  it("does not show the created bot after switching teams", async () => {
    const view = await mount("cloud", storedBot);
    expect(screen.getByRole("region", { name: "Test run" })).toBeTruthy();
    team.id = "team-2";
    view.rerender(<BotBuilderView />);
    expect(screen.queryByRole("region", { name: "Test run" })).toBeNull();
  });

  it("says the pane is unavailable for a local bot with no workspace path", async () => {
    await mount("local", storedBot);
    expect(screen.queryByRole("region", { name: "Test run" })).toBeNull();
    expect(screen.getByText("Test pane unavailable")).toBeTruthy();
  });
});
