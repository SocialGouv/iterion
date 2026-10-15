// @vitest-environment jsdom
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

const conn = {
  id: "c1",
  provider: "gitlab",
  kind: "pat",
  status: "active",
  account_login: "iterion-bot",
  created_at: "2026-10-01T00:00:00Z",
} as ForgeConnection;

const reviewBot = {
  name: "review-pr",
  display_name: "Revi",
  invocations: [{ kind: "forge", forge: { event: "merge_request" } }],
} as unknown as BotEntryWithSchema;

const converseBot = {
  name: "revi-converse",
  display_name: "Revi (converse)",
  invocations: [{ kind: "command", command: { name: "revi" } }],
} as unknown as BotEntryWithSchema;

const preview = (over: Partial<ForgeEnablePreview> = {}): ForgeEnablePreview => ({
  events_normalized: ["merge_request"],
  forge_native_events: ["merge_request"],
  scopes: {},
  secrets: [],
  commands: [],
  identity: { handle: "iterion-bot", provider: "gitlab", base_url: "https://gitlab.example" },
  gates_merges: true,
  hook_base_url: "https://iterion.fabrique.social.gouv.fr",
  conflicts: [],
  ...over,
});

const integration = (over: Partial<ForgeIntegration> = {}) =>
  ({
    id: "i1",
    connection_id: "c1",
    provider: "gitlab",
    repo_full_name: "org/api",
    bot_ids: ["review-pr"],
    events_normalized: ["merge_request"],
    webhook_id: "w1",
    hook_id: "h1",
    created_at: "2026-10-01T00:00:00Z",
    ...over,
  }) as ForgeIntegration;

const newClient = () =>
  new QueryClient({ defaultOptions: { queries: { retry: false } } });

/** The panel under one query client — its own integrations read included. */
function panelIn(client: QueryClient, onDone = vi.fn()) {
  return (
    <QueryClientProvider client={client}>
      <EnableRepoPanel
        teamID="t1"
        conn={conn}
        repoBots={[reviewBot]}
        onDone={onDone}
        onCancel={vi.fn()}
        onError={vi.fn()}
      />
    </QueryClientProvider>
  );
}

function renderPanel() {
  const onDone = vi.fn();
  render(panelIn(newClient(), onDone));
  return { onDone };
}

const checked = (el: HTMLElement) => (el as HTMLInputElement).checked;

/** Pick the repo and the bot — what makes the panel ask for a preview. */
async function selectRepoAndBot() {
  await screen.findByRole("option", { name: /org\/api/ });
  fireEvent.change(screen.getByLabelText("Repository"), { target: { value: "org/api" } });
  fireEvent.click(screen.getByRole("checkbox"));
  await waitFor(() => expect(previewMock).toHaveBeenCalled());
}

beforeEach(() => {
  vi.clearAllMocks();
  reposMock.mockResolvedValue([{ full_name: "org/api", can_admin: true } as ForgeRepo]);
  integrationsMock.mockResolvedValue([]);
  previewMock.mockResolvedValue(preview());
  enableMock.mockResolvedValue({
    integration_id: "i1",
    webhook_id: "w1",
    hook_id: "h1",
    managed_secret_id: "s1",
    bot_ids: ["review-pr"],
    created: true,
  });
});

afterEach(cleanup);

// The merge gate is a REQUIRED check on a head SHA: a bot that posts it also
// re-reviews every push to keep it current (the server forces review_on_sync
// for such a selection). An operator who wants reviews without that cost
// chooses the posture here, at provisioning time — and only here, because a
// re-enable of an already-integrated repo must not restate the pins the repo
// page owns.
describe("EnableRepoPanel — the merge-gate posture", () => {
  it("offers the two postures for a repo this connection does not carry yet", async () => {
    renderPanel();
    await selectRepoAndBot();

    const gate = await screen.findByRole("radio", { name: /Gate merges/ });
    expect(checked(gate), "the server's default posture is the checked one").toBe(true);
    expect(checked(screen.getByRole("radio", { name: /Advisory only/ }))).toBe(false);
    // Each posture says what it costs, in the panel, not in a doc.
    screen.getByText(
      "The bots post a merge-gate status and re-review every push to keep it current.",
    );
    screen.getByText(
      "Reviews when the request opens and on demand, without a merge-gate status.",
    );
  });

  it("pins gate_enabled=false when the admin picks Advisory only", async () => {
    renderPanel();
    await selectRepoAndBot();
    fireEvent.click(await screen.findByRole("radio", { name: /Advisory only/ }));
    fireEvent.click(screen.getByRole("button", { name: "Enable" }));

    await waitFor(() => expect(enableMock).toHaveBeenCalledTimes(1));
    // "false" is a string: launch_vars is map[string]string server-side.
    expect(enableMock.mock.calls[0]).toEqual([
      "t1",
      "c1",
      "org/api",
      ["review-pr"],
      {},
      { gate_enabled: "false" },
    ]);
  });

  it("sends NO launch_vars for the default posture", async () => {
    renderPanel();
    await selectRepoAndBot();
    await screen.findByRole("radio", { name: /Gate merges/ });
    fireEvent.click(screen.getByRole("button", { name: "Enable" }));

    await waitFor(() => expect(enableMock).toHaveBeenCalledTimes(1));
    // Not `{gate_enabled:"true"}`: a nil map leaves the stored pins alone,
    // and the default posture is "nothing pinned".
    expect(enableMock.mock.calls[0]?.[5]).toBeUndefined();
  });

  it("offers no posture when no selected bot posts a status", async () => {
    previewMock.mockResolvedValue(preview({ gates_merges: false }));
    renderPanel();
    await selectRepoAndBot();

    // The preview box is up, so the panel HAS the server's answer…
    await screen.findByText(/Will deliver to:/);
    // …and it says no bot here gates merges: there is nothing to choose.
    expect(screen.queryByRole("radio", { name: /Gate merges/ })).toBeNull();
    expect(screen.queryByRole("radio", { name: /Advisory only/ })).toBeNull();
  });

  it("reads the posture of an already-integrated repo and sends the operator to the repo page", async () => {
    integrationsMock.mockResolvedValue([integration({ launch_vars: { gate_enabled: "false" } })]);
    renderPanel();
    await selectRepoAndBot();

    await screen.findByText(/Merge gate: Advisory only/);
    const link = screen.getByRole("link", { name: "change it on the repo page" });
    expect(link.getAttribute("href")).toBe("/repos/c1%3A%3Aorg%2Fapi");
    // No radios: a re-enable restating launch_vars would drop the repo's
    // other pins (the server refuses such a partial write).
    expect(screen.queryByRole("radio", { name: /Advisory only/ })).toBeNull();
  });

  it("names the address the forge will be told to deliver to", async () => {
    renderPanel();
    await selectRepoAndBot();

    // The pinned base, not the deployment's public URL: a forge whose
    // allowlist refuses that host would 422 the hook, and the operator must
    // see which address Enable is about to register.
    await screen.findByText("https://iterion.fabrique.social.gouv.fr/api/webhooks/gitlab/…");
  });

  it("re-asks the preview when the connection's pinned base changes", async () => {
    // ONE query client across both renders: a fresh cache would refetch for
    // any key, which would prove nothing about this one.
    const client = newClient();
    const panel = (c: ForgeConnection) => (
      <QueryClientProvider client={client}>
        <EnableRepoPanel
          teamID="t1"
          conn={c}
          repoBots={[reviewBot]}
          onDone={vi.fn()}
          onCancel={vi.fn()}
          onError={vi.fn()}
        />
      </QueryClientProvider>
    );
    const { rerender } = render(panel(conn));
    await selectRepoAndBot();
    await screen.findByText("https://iterion.fabrique.social.gouv.fr/api/webhooks/gitlab/…");

    // The row above just pinned another base; the refetched connection flows
    // in as a new prop. The hook address is part of the preview, so an answer
    // cached under a key blind to the pin would keep showing the OLD one.
    previewMock.mockResolvedValue(preview({ hook_base_url: "https://iterion.example.org" }));
    rerender(panel({ ...conn, webhook_base_url: "https://iterion.example.org" }));

    await screen.findByText("https://iterion.example.org/api/webhooks/gitlab/…");
  });
});

// The posture travels only while the operator can see it, and only once the
// panel knows whether the repo is already integrated — which it knows from
// its own read of the team's integrations, on every surface that mounts it.
describe("EnableRepoPanel — when the posture is a choice at all", () => {
  it("does not send an Advisory only the operator no longer sees", async () => {
    previewMock.mockImplementation(async (_team, _conn, _repo, bots) =>
      preview({ gates_merges: bots.includes("review-pr") }),
    );
    render(
      <QueryClientProvider client={newClient()}>
        <EnableRepoPanel
          teamID="t1"
          conn={conn}
          repoBots={[reviewBot, converseBot]}
          onDone={vi.fn()}
          onCancel={vi.fn()}
          onError={vi.fn()}
        />
      </QueryClientProvider>,
    );
    await screen.findByRole("option", { name: /org\/api/ });
    fireEvent.change(screen.getByLabelText("Repository"), { target: { value: "org/api" } });
    fireEvent.click(screen.getByRole("checkbox", { name: /review-pr/ }));
    fireEvent.click(await screen.findByRole("radio", { name: /Advisory only/ }));

    // The gating bot goes, a non-gating one comes: the fieldset leaves the
    // screen, and the choice made in it must leave the request too.
    fireEvent.click(screen.getByRole("checkbox", { name: /review-pr/ }));
    fireEvent.click(screen.getByRole("checkbox", { name: /revi-converse/ }));
    await waitFor(() =>
      expect(screen.queryByRole("radio", { name: /Advisory only/ })).toBeNull(),
    );
    await screen.findByText(/Will deliver to:/);
    fireEvent.click(screen.getByRole("button", { name: "Enable" }));

    await waitFor(() => expect(enableMock).toHaveBeenCalledTimes(1));
    expect(enableMock.mock.calls[0]?.[3]).toEqual(["revi-converse"]);
    expect(enableMock.mock.calls[0]?.[5]).toBeUndefined();
  });

  it("reads the team's integrations itself", async () => {
    integrationsMock.mockResolvedValue([integration({ launch_vars: { gate_enabled: "false" } })]);
    renderPanel();
    await selectRepoAndBot();

    await screen.findByText(/Merge gate: Advisory only/);
    expect(integrationsMock).toHaveBeenCalledWith("t1");
    expect(screen.queryByRole("radio", { name: /Advisory only/ })).toBeNull();
  });

  it("re-reads a cached list instead of deciding on it", async () => {
    // A surface above left a snapshot under the shared key, and the repo it
    // carries has since been disabled. Whether a list is current is a fact
    // about the read, so the panel's own answer is the one it decides on.
    const client = newClient();
    client.setQueryData(
      ["forge-integrations", "t1"],
      [integration({ launch_vars: { gate_enabled: "false" } })],
    );
    integrationsMock.mockResolvedValue([]);
    render(panelIn(client));
    await selectRepoAndBot();

    await screen.findByRole("radio", { name: /Gate merges/ });
    expect(integrationsMock).toHaveBeenCalledWith("t1");
    expect(screen.queryByText(/Merge gate: Advisory only/)).toBeNull();
  });

  it("offers no posture while the team's integrations cannot be read", async () => {
    integrationsMock.mockRejectedValue(new Error("API error 500: store down"));
    renderPanel();
    await selectRepoAndBot();

    await screen.findByText(/this team.s enabled repos could not be read \(API error 500: store down\)/);
    expect(screen.queryByRole("radio", { name: /Advisory only/ })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Enable" }));
    await waitFor(() => expect(enableMock).toHaveBeenCalledTimes(1));
    expect(enableMock.mock.calls[0]?.[5]).toBeUndefined();
  });

  it("names no posture for an integrated repo whose bots do not gate and that pins nothing", async () => {
    previewMock.mockResolvedValue(preview({ gates_merges: false }));
    integrationsMock.mockResolvedValue([integration({ launch_vars: { review_tier: "glance" } })]);
    renderPanel();
    await selectRepoAndBot();

    await screen.findByText(/Will deliver to:/);
    expect(screen.queryByText(/Merge gate:/)).toBeNull();
  });
});

// A posture is a statement about THIS repo: the panel makes it from a read it
// has seen succeed, never from whatever sits in the shared integrations cache
// — the Integrations tab's older snapshot, which react-query also keeps next
// to the error of a refetch that failed.
describe("EnableRepoPanel — the integrations a posture may be read from", () => {
  const panelReadingItself = (client: QueryClient) => (
    <QueryClientProvider client={client}>
      <EnableRepoPanel
        teamID="t1"
        conn={conn}
        repoBots={[reviewBot]}
        onDone={vi.fn()}
        onCancel={vi.fn()}
        onError={vi.fn()}
      />
    </QueryClientProvider>
  );

  it("offers none off a cached list the refetch has not confirmed", async () => {
    const client = newClient();
    // The snapshot an earlier mount left, predating this repo's integration.
    client.setQueryData(["forge-integrations", "t1"], [] as ForgeIntegration[]);
    let release!: (v: ForgeIntegration[]) => void;
    integrationsMock.mockImplementation(
      () => new Promise<ForgeIntegration[]>((res) => (release = res)),
    );
    render(panelReadingItself(client));
    await selectRepoAndBot();

    // The preview HAS landed, so a missing fieldset is about the integrations
    // and nothing else.
    await screen.findByText(/Will deliver to:/);
    expect(screen.queryByRole("radio", { name: /Gate merges/ })).toBeNull();

    // The truth, arriving late: the repo is integrated here, advisory-only,
    // and pins a review tier a restated launch_vars would drop.
    release([integration({ launch_vars: { gate_enabled: "false", review_tier: "audit" } })]);
    await screen.findByText(/Merge gate: Advisory only/);
    expect(screen.queryByRole("radio", { name: /Gate merges/ })).toBeNull();
  });

  it("offers none when the refetch failed over a cached list", async () => {
    const client = newClient();
    client.setQueryData(["forge-integrations", "t1"], [] as ForgeIntegration[]);
    integrationsMock.mockRejectedValue(new Error("API error 500: store down"));
    render(panelReadingItself(client));
    await selectRepoAndBot();

    // The honest line stands alone: radios next to it would contradict it,
    // and this state does not resolve — the read failed.
    await screen.findByText(/this team.s enabled repos could not be read/);
    expect(screen.queryByRole("radio", { name: /Gate merges/ })).toBeNull();
    expect(screen.queryByRole("radio", { name: /Advisory only/ })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Enable" }));
    await waitFor(() => expect(enableMock).toHaveBeenCalledTimes(1));
    expect(enableMock.mock.calls[0]?.[5]).toBeUndefined();
  });

  it("warns instead of naming an address when the deployment has no public URL", async () => {
    // The server keeps hook_base_url empty when it has no public URL to build
    // the hook address from.
    previewMock.mockResolvedValue(preview({ hook_base_url: "" }));
    renderPanel();
    await selectRepoAndBot();

    await screen.findByText(
      "no address — this deployment has no public URL; pin a base URL on the connection above.",
    );
    // And no relative path passed off as an address.
    expect(screen.queryByText(/api\/webhooks\/gitlab/)).toBeNull();
  });

  it("warns, quoting it, when the base is not an absolute http(s) URL", async () => {
    // A public URL that is not an address (a host with no scheme, a path)
    // builds a hook URL every forge refuses — GitLab answers 422 "Invalid url
    // given" — so the panel names the setting to fix instead of announcing it.
    previewMock.mockResolvedValue(preview({ hook_base_url: "iterion.example.org" }));
    renderPanel();
    await selectRepoAndBot();

    await screen.findByText(
      'no address — "iterion.example.org" is not an absolute http(s) URL (auth.publicUrl / ITERION_PUBLIC_URL); pin a base URL on the connection above.',
    );
    expect(screen.queryByText(/iterion\.example\.org\/api\/webhooks/)).toBeNull();
  });

  it("says nothing about an address a server that reports none never gave", async () => {
    // A replica that predates the field answers a preview without it. That is
    // not "no public URL" (the empty string is), and it is not a reason to
    // lose the panel: the delivery line is simply not rendered — the same
    // semantics the bind wizard's Review step has.
    previewMock.mockResolvedValue(
      preview({ hook_base_url: undefined as unknown as string }),
    );
    renderPanel();
    await selectRepoAndBot();

    // The preview landed: the box is up with the rest of the answer…
    await screen.findByText(/Will subscribe to:/);
    // …and the address, which was not in it, is neither announced nor warned
    // about.
    expect(screen.queryByText(/Will deliver to:/)).toBeNull();
    expect(screen.queryByText(/no address/)).toBeNull();
    // Enable provisions: the server validates the base it builds the hook
    // from, and this preview is a fresh answer about this selection.
    const enableBtn = screen.getByRole("button", { name: "Enable" }) as HTMLButtonElement;
    expect(enableBtn.disabled).toBe(false);
  });
});

// A preview that never landed, or landed as an error, is not an answer about
// this selection: Enable registers a hook at the address it carries, so the
// panel neither provisions nor stays mute about why it will not.
describe("EnableRepoPanel — a preview the panel does not have", () => {
  it("refuses Enable while the FIRST preview is in flight, then provisions once it lands", async () => {
    let release!: (p: ForgeEnablePreview) => void;
    previewMock.mockImplementation(
      () => new Promise<ForgeEnablePreview>((res) => (release = res)),
    );
    renderPanel();
    await selectRepoAndBot();

    // Nothing the answer carries is on screen — address, posture, conflicts…
    expect(screen.queryByText(/Will deliver to:/)).toBeNull();
    expect(screen.queryByRole("radio", { name: /Gate merges/ })).toBeNull();
    // …and the button that registers the hook is out.
    const enableBtn = () => screen.getByRole("button", { name: "Enable" }) as HTMLButtonElement;
    expect(enableBtn().disabled).toBe(true);
    fireEvent.click(enableBtn());
    expect(enableMock).not.toHaveBeenCalled();

    // The answer the operator was owed: it says a bot cannot be installed.
    release(preview({ conflicts: ["review-pr: no forge invocation to auto-install"] }));
    await screen.findByText(/no forge invocation to auto-install/);
    expect(enableBtn().disabled).toBe(true);
  });

  it("says why, in the box, when the preview failed — and refuses Enable", async () => {
    previewMock.mockRejectedValue(new Error("API error 502: upstream"));
    renderPanel();
    await selectRepoAndBot();

    await screen.findByText(
      /What this selection would provision could not be read \(API error 502: upstream\) — Enable stays out until it can\./,
    );
    const enableBtn = screen.getByRole("button", { name: "Enable" }) as HTMLButtonElement;
    expect(enableBtn.disabled).toBe(true);
    fireEvent.click(enableBtn);
    expect(enableMock).not.toHaveBeenCalled();
  });
});

// The posture is a statement about ONE repo: the panel's select is also how
// the operator moves to the next one.
describe("EnableRepoPanel — the posture belongs to the repo it was picked for", () => {
  it("starts the next repository on the default posture", async () => {
    reposMock.mockResolvedValue([
      { full_name: "org/api", can_admin: true } as ForgeRepo,
      { full_name: "org/web", can_admin: true } as ForgeRepo,
    ]);
    renderPanel();
    await selectRepoAndBot();
    fireEvent.click(await screen.findByRole("radio", { name: /Advisory only/ }));
    expect(checked(screen.getByRole("radio", { name: /Advisory only/ }))).toBe(true);

    // Another repo, also new on this connection, also gating: the choice made
    // for org/api must not pin org/web advisory-only behind the operator.
    fireEvent.change(screen.getByLabelText("Repository"), { target: { value: "org/web" } });
    await waitFor(() =>
      expect(checked(screen.getByRole("radio", { name: /Gate merges/ }))).toBe(true),
    );
    expect(checked(screen.getByRole("radio", { name: /Advisory only/ }))).toBe(false);

    fireEvent.click(screen.getByRole("button", { name: "Enable" }));
    await waitFor(() => expect(enableMock).toHaveBeenCalledTimes(1));
    expect(enableMock.mock.calls[0]?.[2]).toBe("org/web");
    expect(enableMock.mock.calls[0]?.[5]).toBeUndefined();
  });
});

// keepPreviousData keeps the previous answer on screen while the next one
// loads: it describes the previous selection, or the previous pin. Nothing
// that commits may be read from it — not the announced address, not the
// posture, not Enable.
describe("EnableRepoPanel — a kept preview commits to nothing", () => {
  it("names no address, and refuses Enable, while the new pin's preview is in flight", async () => {
    const client = newClient();
    const panel = (c: ForgeConnection) => (
      <QueryClientProvider client={client}>
        <EnableRepoPanel
          teamID="t1"
          conn={c}
          repoBots={[reviewBot]}
          onDone={vi.fn()}
          onCancel={vi.fn()}
          onError={vi.fn()}
        />
      </QueryClientProvider>
    );
    const { rerender } = render(panel(conn));
    await selectRepoAndBot();
    await screen.findByText("https://iterion.fabrique.social.gouv.fr/api/webhooks/gitlab/…");

    // The row above pinned another base; the answer for it is in flight.
    let release!: (p: ForgeEnablePreview) => void;
    previewMock.mockImplementation(
      () => new Promise<ForgeEnablePreview>((res) => (release = res)),
    );
    rerender(panel({ ...conn, webhook_base_url: "https://iterion.example.org" }));
    await waitFor(() => expect(previewMock).toHaveBeenCalledTimes(2));

    // The hook would be registered under the pin, not under the address the
    // box held: say nothing, and keep the button that registers it out.
    await waitFor(() => expect(screen.queryByText(/api\/webhooks\/gitlab/)).toBeNull());
    const enableBtn = screen.getByRole("button", { name: "Enable" }) as HTMLButtonElement;
    expect(enableBtn.disabled).toBe(true);
    fireEvent.click(enableBtn);
    expect(enableMock).not.toHaveBeenCalled();

    release(preview({ hook_base_url: "https://iterion.example.org" }));
    await screen.findByText("https://iterion.example.org/api/webhooks/gitlab/…");
  });

  it("offers no posture read from the previous selection's answer", async () => {
    previewMock.mockImplementation(async (_team, _conn, _repo, bots) =>
      preview({ gates_merges: bots.includes("review-pr") }),
    );
    render(
      <QueryClientProvider client={newClient()}>
        <EnableRepoPanel
          teamID="t1"
          conn={conn}
          repoBots={[reviewBot, converseBot]}
          onDone={vi.fn()}
          onCancel={vi.fn()}
          onError={vi.fn()}
        />
      </QueryClientProvider>,
    );
    await screen.findByRole("option", { name: /org\/api/ });
    fireEvent.change(screen.getByLabelText("Repository"), { target: { value: "org/api" } });
    fireEvent.click(screen.getByRole("checkbox", { name: /review-pr/ }));
    fireEvent.click(await screen.findByRole("radio", { name: /Advisory only/ }));

    // The gating bot goes, a non-gating one comes, and the answer for THAT
    // selection is in flight: the fieldset on screen belongs to the old one.
    let release!: (p: ForgeEnablePreview) => void;
    previewMock.mockImplementation(
      () => new Promise<ForgeEnablePreview>((res) => (release = res)),
    );
    fireEvent.click(screen.getByRole("checkbox", { name: /review-pr/ }));
    fireEvent.click(screen.getByRole("checkbox", { name: /revi-converse/ }));
    await waitFor(() => expect(previewMock).toHaveBeenCalledTimes(2));

    await waitFor(() =>
      expect(screen.queryByRole("radio", { name: /Advisory only/ })).toBeNull(),
    );
    const enableBtn = () => screen.getByRole("button", { name: "Enable" }) as HTMLButtonElement;
    expect(enableBtn().disabled).toBe(true);
    fireEvent.click(enableBtn());
    expect(enableMock).not.toHaveBeenCalled();

    // The fresh answer: this selection posts no status, so there is no
    // posture — and none travels with the enable.
    release(preview({ gates_merges: false }));
    await waitFor(() => expect(enableBtn().disabled).toBe(false));
    fireEvent.click(enableBtn());
    await waitFor(() => expect(enableMock).toHaveBeenCalledTimes(1));
    expect(enableMock.mock.calls[0]?.[3]).toEqual(["revi-converse"]);
    expect(enableMock.mock.calls[0]?.[5]).toBeUndefined();
  });
});

// The Integrations tab reads the team's integrations again whenever the window
// regains focus. While that read is in flight the posture cannot be decided,
// and a click must not send the default posture in place of the one the
// operator picked: Enable waits, says why, and the choice survives the read.
describe("EnableRepoPanel — a re-read of the integrations", () => {
  it("holds Enable while the integrations are read again, and keeps the chosen posture", async () => {
    const client = newClient();
    render(
      <QueryClientProvider client={client}>
        <EnableRepoPanel
          teamID="t1"
          conn={conn}
          repoBots={[reviewBot]}
          onDone={vi.fn()}
          onCancel={vi.fn()}
          onError={vi.fn()}
        />
      </QueryClientProvider>,
    );
    await selectRepoAndBot();
    fireEvent.click(await screen.findByRole("radio", { name: /Advisory only/ }));

    let settle: (v: ForgeIntegration[]) => void = () => {};
    integrationsMock.mockImplementationOnce(
      () => new Promise<ForgeIntegration[]>((resolve) => (settle = resolve)),
    );
    void client.invalidateQueries({ queryKey: ["forge-integrations", "t1"] });

    await screen.findByText(/reading this team.s enabled repos/);
    const enableButton = screen.getByRole("button", { name: "Enable" }) as HTMLButtonElement;
    expect(enableButton.disabled, "Enable must wait while the posture cannot be decided").toBe(true);
    fireEvent.click(enableButton);
    expect(enableMock).not.toHaveBeenCalled();

    settle([]);
    await waitFor(() => expect(enableButton.disabled).toBe(false));
    expect(checked(screen.getByRole("radio", { name: /Advisory only/ }))).toBe(true);
    fireEvent.click(enableButton);
    await waitFor(() => expect(enableMock).toHaveBeenCalledTimes(1));
    expect(enableMock.mock.calls[0]?.[5]).toEqual({ gate_enabled: "false" });
  });
});
