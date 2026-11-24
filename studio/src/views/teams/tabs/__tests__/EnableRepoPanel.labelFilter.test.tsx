// @vitest-environment jsdom
// The repo's issue-label filter, in the Enable panel.
//
// pkg/server reads `label_allowlist` three ways on the repo-bots writes
// (forgeEnableReq / forgeUpdateReq): ABSENT keeps the list the repo holds, an
// EMPTY list clears it — every freshly applied label dispatches again, the
// fail-open direction — and a non-empty list replaces it. The panel therefore
// owes two things: the field shows the list the repo holds, and Enable states
// the filter only as a change. These witnesses read the seventh argument of
// enableForgeRepoBots, the one the client writes as `label_allowlist`.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import type { BotEntryWithSchema } from "@/api/bots";
import type {
  ForgeConnection,
  ForgeEnablePreview,
  ForgeIntegration,
  ForgeRepo,
} from "@/api/forgeConnections";

vi.mock("@/api/forgeConnections", async () => {
  const actual =
    await vi.importActual<typeof import("@/api/forgeConnections")>("@/api/forgeConnections");
  return {
    ...actual,
    listForgeIntegrations: vi.fn(),
    listForgeRepos: vi.fn(),
    previewForgeEnable: vi.fn(),
    enableForgeRepoBots: vi.fn(),
  };
});

import {
  enableForgeRepoBots,
  listForgeIntegrations,
  listForgeRepos,
  previewForgeEnable,
} from "@/api/forgeConnections";

import { EnableRepoPanel } from "../integrations/EnableRepoPanel";

const reposMock = vi.mocked(listForgeRepos);
const previewMock = vi.mocked(previewForgeEnable);
const enableMock = vi.mocked(enableForgeRepoBots);
const integrationsMock = vi.mocked(listForgeIntegrations);

const FIELD = "Start only on these issue labels";
const INTEGRATIONS_KEY = ["forge-integrations", "t1"];

const conn = {
  id: "c1",
  provider: "github",
  kind: "pat",
  status: "active",
  account_login: "iterion-bot",
  created_at: "2026-10-01T00:00:00Z",
} as ForgeConnection;

const implementBot = {
  name: "featurly",
  display_name: "Featurly",
  invocations: [{ kind: "forge", forge: { event: "issues" } }],
} as unknown as BotEntryWithSchema;

// A selection that gates no merge: the posture then holds nothing back, so
// what Enable waits for in these tests is the label filter alone.
const preview = (): ForgeEnablePreview => ({
  events_normalized: ["issues"],
  forge_native_events: ["issues"],
  scopes: {},
  secrets: [],
  commands: [],
  identity: { handle: "iterion-bot", provider: "github", base_url: "https://github.com" },
  gates_merges: false,
  hook_base_url: "https://iterion.example.org",
  conflicts: [],
});

const integration = (over: Partial<ForgeIntegration> = {}) =>
  ({
    id: "i1",
    connection_id: "c1",
    provider: "github",
    repo_full_name: "org/api",
    bot_ids: ["featurly"],
    events_normalized: ["issues"],
    webhook_id: "w1",
    hook_id: "h1",
    created_at: "2026-10-01T00:00:00Z",
    ...over,
  }) as ForgeIntegration;

const newClient = () => new QueryClient({ defaultOptions: { queries: { retry: false } } });

function renderPanel(client: QueryClient = newClient()) {
  render(
    <QueryClientProvider client={client}>
      <EnableRepoPanel
        teamID="t1"
        conn={conn}
        repoBots={[implementBot]}
        onDone={vi.fn()}
        onCancel={vi.fn()}
        onError={vi.fn()}
      />
    </QueryClientProvider>,
  );
  return client;
}

/** Pick the repo and the bot, and wait for the answer Enable provisions from. */
async function selectRepoAndBot(repo = "org/api") {
  await screen.findByRole("option", { name: new RegExp(repo) });
  fireEvent.change(screen.getByLabelText("Repository"), { target: { value: repo } });
  fireEvent.click(screen.getByRole("checkbox"));
  await screen.findByText(/Will deliver to:/);
}

const field = () => screen.findByLabelText(FIELD);

/** Type a label and commit it the way an operator does: Enter. */
async function addLabel(text: string) {
  const input = await field();
  fireEvent.change(input, { target: { value: text } });
  fireEvent.keyDown(input, { key: "Enter" });
}

/** The labels on screen, read off the chips' remove buttons. */
const chips = () =>
  screen
    .queryAllByRole("button", { name: /^Remove / })
    .map((b) => (b.getAttribute("aria-label") ?? "").replace(/^Remove /, ""));

const enableButton = () => screen.getByRole("button", { name: "Enable" }) as HTMLButtonElement;

/** Click Enable and return what the request states about the label filter. */
async function enableAndReadFilter() {
  fireEvent.click(enableButton());
  await waitFor(() => expect(enableMock).toHaveBeenCalledTimes(1));
  return enableMock.mock.calls[0]?.[6];
}

/** The line naming the list the repo holds, as one string. */
const storedLine = () =>
  screen.queryByText(/Label filter on this repo:/)?.textContent ?? null;

beforeEach(() => {
  vi.clearAllMocks();
  reposMock.mockResolvedValue([
    { full_name: "org/api", can_admin: true } as ForgeRepo,
    { full_name: "org/web", can_admin: true } as ForgeRepo,
  ]);
  integrationsMock.mockResolvedValue([]);
  previewMock.mockResolvedValue(preview());
  enableMock.mockResolvedValue({
    integration_id: "i1",
    webhook_id: "w1",
    hook_id: "h1",
    managed_secret_id: "s1",
    bot_ids: ["featurly"],
    created: true,
  });
});

afterEach(cleanup);

describe("EnableRepoPanel — the label filter of a repo enabled for the first time", () => {
  it("sends the labels the operator typed, trimmed, without duplicates or blanks", async () => {
    renderPanel();
    await selectRepoAndBot();

    await addLabel("  securite ");
    await addLabel("bug");
    // The server matches labels case-insensitively: a second spelling of one
    // already listed is the same entry.
    await addLabel("Securite");
    await addLabel("   ");
    expect(chips()).toEqual(["securite", "bug"]);

    expect(await enableAndReadFilter()).toEqual(["securite", "bug"]);
  });

  it("sends a long label whole", async () => {
    // GitLab allows 255 characters in a label. The chip input cuts a tag to
    // 64 unless told otherwise, and a cut entry is a filter that never
    // matches its label.
    const long = `team/${"x".repeat(120)}`;
    renderPanel();
    await selectRepoAndBot();
    await addLabel(long);

    expect(await enableAndReadFilter()).toEqual([long]);
  });

  it("states no filter when the field stays empty", async () => {
    renderPanel();
    await selectRepoAndBot();
    await field();
    // What the empty field means is said next to it.
    screen.getByText(/Empty = any issue label the bots declare starts them\./);

    // Not `[]`: the key is absent, so the server narrows nothing and clears
    // nothing — the behaviour of a panel without the field.
    const sent = await enableAndReadFilter();
    expect(sent).toBeUndefined();
  });

  it("states no filter for labels typed and then all removed", async () => {
    renderPanel();
    await selectRepoAndBot();
    await addLabel("securite");
    fireEvent.click(screen.getByRole("button", { name: "Remove securite" }));
    expect(chips()).toEqual([]);

    expect(await enableAndReadFilter()).toBeUndefined();
  });

  it("does not carry a filter typed for one repository into the next", async () => {
    renderPanel();
    await selectRepoAndBot();
    await addLabel("securite");
    expect(chips()).toEqual(["securite"]);

    fireEvent.change(screen.getByLabelText("Repository"), { target: { value: "org/web" } });
    await waitFor(() => expect(chips()).toEqual([]));
    await waitFor(() => expect(enableButton().disabled).toBe(false));

    fireEvent.click(enableButton());
    await waitFor(() => expect(enableMock).toHaveBeenCalledTimes(1));
    expect(enableMock.mock.calls[0]?.[2]).toBe("org/web");
    expect(enableMock.mock.calls[0]?.[6]).toBeUndefined();
  });
});

describe("EnableRepoPanel — a list of labels that arrives as one text", () => {
  // The chip input reads a comma as a separator on the key only: a pasted
  // "bug, security" is committed as ONE chip. Stored as one label it matches
  // neither on the webhook side, so the field reads it as the list it is.
  it("reads a pasted comma-separated text as one label per entry", async () => {
    renderPanel();
    await selectRepoAndBot();
    await addLabel("bug, security");
    expect(chips()).toEqual(["bug", "security"]);

    expect(await enableAndReadFilter()).toEqual(["bug", "security"]);
  });

  it("drops blanks and the entries already listed, whatever their case", async () => {
    renderPanel();
    await selectRepoAndBot();
    await addLabel("bug");
    await addLabel("Bug,, urgent , ,security");

    expect(await enableAndReadFilter()).toEqual(["bug", "urgent", "security"]);
  });

  it("keeps whole a stored label that holds a comma, while new entries are split", async () => {
    // A label set through the API may hold a comma (GitHub allows it): an
    // edit of the field must not rewrite an entry the operator did not type.
    integrationsMock.mockResolvedValue([integration({ label_allowlist: ["won't fix, duplicate"] })]);
    renderPanel();
    await selectRepoAndBot();
    await addLabel("bug, security");

    expect(await enableAndReadFilter()).toEqual(["won't fix, duplicate", "bug", "security"]);
  });

  it("splits a text equal to a stored comma label once that label was removed", async () => {
    // The comma label is kept whole only while the field holds it: entered
    // again after its removal it is a new entry, read like any other.
    integrationsMock.mockResolvedValue([
      integration({ label_allowlist: ["won't fix, duplicate", "bug"] }),
    ]);
    renderPanel();
    await selectRepoAndBot();
    fireEvent.click(await screen.findByRole("button", { name: "Remove won't fix, duplicate" }));
    await addLabel("won't fix, duplicate");

    expect(await enableAndReadFilter()).toEqual(["bug", "won't fix", "duplicate"]);
  });

  it("takes a pasted list longer than one label's length without cutting it", async () => {
    const labels = Array.from({ length: 40 }, (_, i) => `team/label-number-${i}`);
    renderPanel();
    await selectRepoAndBot();
    await addLabel(labels.join(", "));

    expect(await enableAndReadFilter()).toEqual(labels);
  });
});

describe("EnableRepoPanel — the label filter of a repo already enabled", () => {
  beforeEach(() => {
    integrationsMock.mockResolvedValue([integration({ label_allowlist: ["securite", "bug"] })]);
  });

  it("shows the list the repo holds, in the field and in a line naming it", async () => {
    renderPanel();
    await selectRepoAndBot();
    await field();

    expect(chips()).toEqual(["securite", "bug"]);
    expect(storedLine()).toBe("Label filter on this repo: securite, bug");
  });

  it("states nothing about the filter when the field is left untouched", async () => {
    renderPanel();
    await selectRepoAndBot();
    await field();
    expect(chips()).toEqual(["securite", "bug"]);

    // An echoed list would be a write: omitted, the server keeps its own.
    expect(await enableAndReadFilter()).toBeUndefined();
  });

  it("sends an EMPTY list — the server's clearing form — once every label is removed", async () => {
    renderPanel();
    await selectRepoAndBot();
    await field();
    fireEvent.click(screen.getByRole("button", { name: "Remove securite" }));
    fireEvent.click(screen.getByRole("button", { name: "Remove bug" }));
    expect(chips()).toEqual([]);
    // The widening is announced before the click that makes it.
    expect(storedLine()).toBe("Label filter on this repo: securite, bug — Enable removes it.");

    const sent = await enableAndReadFilter();
    expect(sent, "an omitted key keeps the stored filter instead of clearing it").toEqual([]);
  });

  it("sends the whole new list when a label is added", async () => {
    renderPanel();
    await selectRepoAndBot();
    await addLabel("urgent");
    expect(storedLine()).toBe("Label filter on this repo: securite, bug — Enable replaces it.");

    expect(await enableAndReadFilter()).toEqual(["securite", "bug", "urgent"]);
  });

  it("sends the remaining list when a label is removed", async () => {
    renderPanel();
    await selectRepoAndBot();
    await field();
    fireEvent.click(screen.getByRole("button", { name: "Remove bug" }));

    expect(await enableAndReadFilter()).toEqual(["securite"]);
  });

  it("states nothing once an edit is undone back to the stored list", async () => {
    renderPanel();
    await selectRepoAndBot();
    await addLabel("urgent");
    fireEvent.click(screen.getByRole("button", { name: "Remove urgent" }));
    expect(chips()).toEqual(["securite", "bug"]);
    expect(storedLine()).toBe("Label filter on this repo: securite, bug");

    expect(await enableAndReadFilter()).toBeUndefined();
  });
});

// The field shows the list the repo holds, so it is first offered from a
// settled read of the team's integrations — the rule the merge-gate posture
// follows in this panel. An edit the operator already made is another matter:
// it stays on screen through later reads, and travels.
describe("EnableRepoPanel — a label filter the panel cannot read", () => {
  const CHECKING = "Label filter: checking whether this repo already holds one…";
  const UNKNOWN = "Label filter: the one this repo may hold is unknown (API error 500: store down)";

  it("offers no field when the integrations cannot be read, and says Enable keeps the stored one", async () => {
    integrationsMock.mockRejectedValue(new Error("API error 500: store down"));
    renderPanel();
    await selectRepoAndBot();

    await screen.findByText(`${UNKNOWN}, so none is offered — Enable keeps it.`);
    expect(screen.queryByLabelText(FIELD)).toBeNull();

    expect(await enableAndReadFilter()).toBeUndefined();
  });

  it("offers no field, and states no filter, while the first read is in flight", async () => {
    integrationsMock.mockImplementation(() => new Promise<ForgeIntegration[]>(() => {}));
    renderPanel();
    await selectRepoAndBot();

    await screen.findByText(CHECKING);
    expect(screen.queryByLabelText(FIELD)).toBeNull();

    expect(await enableAndReadFilter()).toBeUndefined();
  });

  it("takes an untouched field off screen for a re-read, and names no stored list meanwhile", async () => {
    integrationsMock.mockResolvedValue([integration({ label_allowlist: ["securite"] })]);
    const client = renderPanel();
    await selectRepoAndBot();
    await field();
    expect(storedLine()).toBe("Label filter on this repo: securite");

    integrationsMock.mockImplementationOnce(() => new Promise<ForgeIntegration[]>(() => {}));
    void client.invalidateQueries({ queryKey: INTEGRATIONS_KEY });

    // The list on screen was the stored one, and whether it still is is what
    // the read in flight will say.
    await screen.findByText(CHECKING);
    expect(screen.queryByLabelText(FIELD)).toBeNull();
    expect(storedLine()).toBeNull();

    expect(await enableAndReadFilter()).toBeUndefined();
  });

  it("keeps an edited filter on screen through a re-read, and sends it", async () => {
    const client = renderPanel();
    await selectRepoAndBot();
    await addLabel("securite");

    integrationsMock.mockImplementationOnce(() => new Promise<ForgeIntegration[]>(() => {}));
    void client.invalidateQueries({ queryKey: INTEGRATIONS_KEY });
    await screen.findByText(CHECKING);

    // The edit neither leaves the screen nor waits on a read that may never
    // settle: what the operator typed is what Enable sends.
    expect(chips()).toEqual(["securite"]);
    expect(enableButton().disabled).toBe(false);
    expect(await enableAndReadFilter()).toEqual(["securite"]);
  });

  it("keeps an edited filter on screen when the re-read fails, and says what Enable does with it", async () => {
    integrationsMock.mockResolvedValue([integration({ label_allowlist: ["securite"] })]);
    const client = renderPanel();
    await selectRepoAndBot();
    await addLabel("bug");

    integrationsMock.mockRejectedValueOnce(new Error("API error 500: store down"));
    void client.invalidateQueries({ queryKey: INTEGRATIONS_KEY });

    await screen.findByText(`${UNKNOWN} — Enable replaces it.`);
    expect(chips()).toEqual(["securite", "bug"]);
    // The stored list is named from a settled read only.
    expect(storedLine()).toBeNull();

    expect(await enableAndReadFilter()).toEqual(["securite", "bug"]);
  });
});
