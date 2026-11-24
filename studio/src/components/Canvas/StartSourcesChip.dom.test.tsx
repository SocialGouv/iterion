// @vitest-environment jsdom
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { BotEntryWithSchema } from "@/api/bots";
import { FeatureUnavailableError, botSourceEditorPath } from "@/api/client";
import type { TriggerSubscription } from "@/api/triggers";
import { useBotsStore } from "@/store/bots";
import { documentStore } from "@/store/document";
import { useServerInfoStore } from "@/store/serverInfo";

const listTriggers = vi.fn();
vi.mock("@/api/triggers", async (importOriginal) => ({
  ...(await importOriginal<typeof import("@/api/triggers")>()),
  listTriggers: (...args: unknown[]) => listTriggers(...args),
}));

import { resolveOpenBot, sourcesOf } from "@/lib/startSources";
import { StartSourcesChip } from "./StartSourcesChip";

const BUNDLE = { name: "triage", path: "/w/bots/triage", rel_path: "bots/triage", is_bundle: true } as BotEntryWithSchema;
const LOOSE = { name: "loose", path: "/w/bots/loose.bot", rel_path: "bots/loose.bot", is_bundle: false } as BotEntryWithSchema;
const TEAM = { name: "teamonly", path: "", origin: "tenant", is_bundle: true } as BotEntryWithSchema;

function sub(over: Partial<TriggerSubscription>): TriggerSubscription {
  return { id: "s", bot_id: "triage", invocation: "connector", enabled: true, match: {}, ...over } as TriggerSubscription;
}

function renderChip() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={client}>
      <StartSourcesChip />
    </QueryClientProvider>,
  );
  // The query's own state, so an assertion waits for the answer it judges.
  const settled = () => client.getQueryCache().findAll({ queryKey: ["triggers", "bot"] })[0]?.state.status;
  return { ...view, settled };
}

beforeEach(() => {
  listTriggers.mockReset();
  useBotsStore.setState({ bots: [BUNDLE, LOOSE, TEAM], fetch: async () => {} });
  documentStore.setState({ currentFilePath: "bots/triage/main.bot" });
  useServerInfoStore.setState({ info: { mode: "cloud" } as never });
});
afterEach(cleanup);

describe("resolveOpenBot", () => {
  it("is the inverse of every opener: a bundle, a loose bot, a team's bot from its source", () => {
    const bots = [BUNDLE, LOOSE, TEAM];
    expect(resolveOpenBot(bots, "bots/triage/main.bot", "team-1")?.name).toBe("triage");
    expect(resolveOpenBot(bots, "bots/loose.bot", "team-1")?.name).toBe("loose");
    expect(resolveOpenBot(bots, botSourceEditorPath("team-1", "teamonly"), "team-1")?.name).toBe("teamonly");
  });
  it("finds nothing that is no bot of the active team", () => {
    const bots = [BUNDLE, LOOSE, TEAM];
    expect(resolveOpenBot(bots, botSourceEditorPath("team-2", "teamonly"), "team-1")).toBeUndefined();
    expect(resolveOpenBot(bots, botSourceEditorPath("team-1", "teamonly", "prompts/a.md"), "team-1")).toBeUndefined();
    expect(resolveOpenBot(bots, "bots/triage/other.bot", "team-1")).toBeUndefined();
    // A bot whose path the server blanked never matches an empty one.
    expect(resolveOpenBot([TEAM], "", "team-1")).toBeUndefined();
    expect(resolveOpenBot(null, "bots/triage/main.bot", "team-1")).toBeUndefined();
  });
});

describe("sourcesOf", () => {
  it("names each enabled source once — a run-finished trigger by the source it matches — and marks a failing one", () => {
    const got = sourcesOf([
      sub({ trigger_id: "slack.message.posted" }),
      sub({ trigger_id: "slack.reaction.added", last_error: "poller: the connection lacks action" }),
      sub({ trigger_id: "jira.issue.changed" }),
      sub({ trigger_id: "rss.item.published", enabled: false }),
      sub({ invocation: "schedule" }),
      sub({ invocation: "" as never, match: { sources: ["run"] } }),
      sub({ invocation: "" as never, match: {} }),
    ]);
    expect(got).toEqual([
      { kind: "connector", id: "jira", failing: false },
      { kind: "connector", id: "slack", failing: true },
      { kind: "family", id: "fin de run", failing: false },
      { kind: "family", id: "planifié", failing: false },
    ]);
  });
});

describe("StartSourcesChip", () => {
  it("shows the bot's sources and, in cloud, a link to plug another carrying the bot", async () => {
    listTriggers.mockResolvedValue([sub({ trigger_id: "slack.message.posted" }), sub({ invocation: "schedule" })]);
    renderChip();
    expect(await screen.findByRole("img", { name: "Sources : slack, planifié" })).toBeTruthy();
    expect(listTriggers).toHaveBeenCalledWith({ bot: "triage" });
    expect(screen.getByRole("link", { name: "+ source" }).getAttribute("href")).toBe("/sources/plug?bot=triage");
  });

  it("offers no plug link where the server plugs no source (local)", async () => {
    useServerInfoStore.setState({ info: { mode: "local" } as never });
    listTriggers.mockResolvedValue([sub({ trigger_id: "slack.message.posted" })]);
    renderChip();
    expect(await screen.findByRole("img", { name: "Sources : slack" })).toBeTruthy();
    expect(screen.queryByRole("link")).toBeNull();
  });

  it("offers to plug the first source of a bot with none — once it knows, never while it asks", async () => {
    let answer: (subs: TriggerSubscription[]) => void = () => {};
    listTriggers.mockReturnValue(new Promise((resolve) => (answer = resolve)));
    const { container } = renderChip();
    await waitFor(() => expect(listTriggers).toHaveBeenCalled());
    expect(container.textContent).toBe("");
    answer([]);
    expect(await screen.findByRole("link", { name: "Brancher une source" })).toBeTruthy();
    expect(screen.queryByRole("img")).toBeNull();
  });

  it("shows nothing for a file that is no registry bot", () => {
    documentStore.setState({ currentFilePath: "scratch/draft.bot" });
    const { container } = renderChip();
    expect(container.textContent).toBe("");
    expect(listTriggers).not.toHaveBeenCalled();
  });

  it("shows nothing on a server with no trigger store, and says so on any other failure", async () => {
    listTriggers.mockRejectedValue(new FeatureUnavailableError("triggers"));
    const { container, unmount, settled } = renderChip();
    await waitFor(() => expect(settled()).toBe("error"));
    expect(container.textContent).toBe("");
    unmount();
    listTriggers.mockRejectedValue(new Error("boom"));
    renderChip();
    expect(await screen.findByText("Sources illisibles")).toBeTruthy();
  });
});
