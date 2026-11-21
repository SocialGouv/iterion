// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

let search = "";
const setLocation = vi.fn();
vi.mock("wouter", () => ({
  useSearch: () => search,
  useLocation: () => ["/sources/plug", setLocation],
  Link: ({ children }: { children: React.ReactNode }) => <>{children}</>,
}));
vi.mock("@/auth/AuthContext", () => ({
  useAuth: () => ({ activeTeam: { team_id: "t1" } }),
}));
const useServerInfoStoreMock = vi.fn(() => "cloud");
vi.mock("@/store/serverInfo", () => ({
  useServerInfoStore: (sel: (m: { info?: { mode?: string } }) => unknown) =>
    sel({ info: { mode: useServerInfoStoreMock() as string } }),
}));

const listSources = vi.fn();
const listConnections = vi.fn();
const listBots = vi.fn();
const previewTrigger = vi.fn();
const createTrigger = vi.fn();
const listWebhooks = vi.fn();
const createWebhook = vi.fn();
const patchConnection = vi.fn();

// The real presentation helpers (delivery labels, the contract's wording):
// only the network is doubled.
vi.mock("@/api/sources", async () => {
  const actual = await vi.importActual<typeof import("@/api/sources")>("@/api/sources");
  return {
    ...actual,
    listSources: (...a: unknown[]) => listSources(...a),
    sourceIconURL: (id: string) => `/api/v1/connectors/${id}/icon`,
  };
});
vi.mock("@/api/connections", () => ({
  listConnections: (...a: unknown[]) => listConnections(...a),
  patchConnection: (...a: unknown[]) => patchConnection(...a),
}));
vi.mock("@/api/bots", () => ({
  listBots: (...a: unknown[]) => listBots(...a),
}));
vi.mock("@/api/triggers", () => ({
  createTrigger: (...a: unknown[]) => createTrigger(...a),
  previewTrigger: (...a: unknown[]) => previewTrigger(...a),
}));
vi.mock("@/api/webhooks", () => ({
  createWebhook: (...a: unknown[]) => createWebhook(...a),
  listWebhooks: (...a: unknown[]) => listWebhooks(...a),
}));

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import PlugWizardView from "./PlugWizardView";

function renderWizard() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  const tree = () => (
    <QueryClientProvider client={client}>
      <PlugWizardView />
    </QueryClientProvider>
  );
  const r = render(tree());
  // goto moves the URL the way a step's own navigation would, keeping the
  // mounted wizard — and the draft state it holds.
  const goto = (next: string) => {
    search = next;
    r.rerender(tree());
  };
  return { ...r, goto };
}
import type { Source } from "@/api/sources";

const SOURCE: Source = {
  kind: "connector",
  id: "probe",
  display_name: "Probe",
  category: "source_control",
  maturity: "experimental",
  tier: "team",
  delivery: "push",
  connections: 1,
  subscriptions: 0,
  setup: "Add the hook at the vendor.",
  triggers: [
    {
      id: "probe.issue.commented",
      display_name: "Issue comment",
      delivery: "push",
      outputs: [{ name: "prompt", type: "string" }],
      filters: [{ id: "author", type: "enum", options: ["alice", "bob"] }],
    },
  ],
};

// The mixed package's two setup notes: its webhook steps and its polling ones.
const MIX_SETUP = "Add the webhook at the vendor, then paste the secret.";
const MIX_SETUP_POLL = "Grant action to the connection; there is no webhook to add.";
const FEED_SETUP = "Give the feed's own address as the instance URL.";

// A card mixing a webhook trigger and a polled one whose poller is keyed by
// a typed query.
const MIXED: Source = {
  kind: "connector",
  id: "mix",
  display_name: "Mix",
  delivery: "mixed",
  connections: 1,
  subscriptions: 0,
  setup: MIX_SETUP,
  setup_poll: MIX_SETUP_POLL,
  triggers: [
    {
      id: "mix.issue.updated",
      display_name: "Issue updated",
      delivery: "push",
      outputs: [{ name: "subject", type: "string" }],
    },
    {
      id: "mix.issue.polled",
      display_name: "Issue changed (polled)",
      delivery: "poll",
      interval: "5m",
      contract: "Polled: sees the revisions it observes at each read.",
      query: [
        { id: "jql", type: "string", required: true, description: "A bounded JQL filter." },
        { id: "limit", type: "int" },
        { id: "archived", type: "bool" },
      ],
      outputs: [{ name: "subject", type: "string" }],
    },
  ],
};

// A polled-only package: no ingress, a query-less trigger.
const FEED: Source = {
  kind: "connector",
  id: "feed",
  display_name: "Feed",
  delivery: "poll",
  connections: 1,
  subscriptions: 0,
  auth: [{ id: "none", kind: "none" }],
  setup: FEED_SETUP,
  triggers: [
    {
      id: "feed.item.published",
      display_name: "Entry published",
      delivery: "poll",
      interval: "15m",
      outputs: [{ name: "title", type: "string" }],
      filters: [{ id: "category", type: "list" }],
    },
  ],
};

beforeEach(() => {
  vi.clearAllMocks();
  // clearAllMocks keeps a mockReturnValue: the local-mode witness must not
  // leak its mode into the tests after it.
  useServerInfoStoreMock.mockReturnValue("cloud");
  search = "?source=probe&step=account";
  listSources.mockResolvedValue([SOURCE, MIXED, FEED]);
  listConnections.mockImplementation(async (_team: string, connector: string) => [
    {
      id: "conn-1",
      alias: "main",
      base_url: `https://${connector}.example`,
      connector,
      capabilities: ["action", "trigger"],
    },
  ]);
  patchConnection.mockResolvedValue({});
  listBots.mockResolvedValue([{ name: "steward" }]);
  listWebhooks.mockResolvedValue([]);
  createWebhook.mockResolvedValue({
    config: { id: "wh-1", provider: "connector" },
    token: "one-time-secret",
  });
  createTrigger.mockResolvedValue({});
  previewTrigger.mockResolvedValue({ matched: true, sample: true, vars: { prompt: "ship it" } });
});

afterEach(cleanup);

describe("PlugWizardView", () => {
  it("refuses an unknown source", async () => {
    search = "?source=nope";
    renderWizard();
    await waitFor(() => screen.getByText("Source inconnue"));
    expect(createTrigger).not.toHaveBeenCalled();
  });

  it("carries the picked connection into the event step's URL", async () => {
    renderWizard();
    await screen.findByText("main");
    fireEvent.click(screen.getByRole("radio", { name: /main/ }));
    expect(setLocation).toHaveBeenCalledWith(
      "/sources/plug?source=probe&step=event&connection=conn-1",
      { replace: true },
    );
  });

  it("previews the sample through the lane's own judgment at the mapping step", async () => {
    search = "?source=probe&step=mapping&connection=conn-1&trigger=probe.issue.commented&bot=steward";
    renderWizard();
    await screen.findByPlaceholderText(/variable recevant prompt/);
    fireEvent.click(screen.getByRole("button", { name: /Prévisualiser/ }));
    await waitFor(() => screen.getByText(/Correspond/));
    expect(previewTrigger).toHaveBeenCalledWith({
      bot_id: "steward",
      connection_id: "conn-1",
      trigger_id: "probe.issue.commented",
      filters: {},
      var_map: undefined,
    });
  });

  it("enables with the connector binding and the minted webhook", async () => {
    search = "?source=probe&step=review&connection=conn-1&trigger=probe.issue.commented&bot=steward";
    renderWizard();
    fireEvent.click(await screen.findByRole("button", { name: /Activer/ }));
    await waitFor(() => expect(createTrigger).toHaveBeenCalled());
    expect(createWebhook).toHaveBeenCalledWith("t1", {
      name: "Probe — probe.issue.commented",
      provider: "connector",
      connector_connection_id: "conn-1",
    });
    expect(createTrigger).toHaveBeenCalledWith(
      expect.objectContaining({
        bot_id: "steward",
        match: {},
        invocation: "connector",
        connection_id: "conn-1",
        trigger_id: "probe.issue.commented",
        enabled: true,
      }),
    );
  });

  it("reuses the connection's webhook instead of minting a second one", async () => {
    listWebhooks.mockResolvedValue([{ id: "wh-existing", connector_connection_id: "conn-1" }]);
    search = "?source=probe&step=review&connection=conn-1&trigger=probe.issue.commented&bot=steward";
    renderWizard();
    fireEvent.click(await screen.findByRole("button", { name: /Activer/ }));
    await waitFor(() => expect(createTrigger).toHaveBeenCalled());
    expect(createWebhook).not.toHaveBeenCalled();
  });
});

describe("PlugWizardView — the bot page's entry", () => {
  it("picks a source first and keeps the bot it carried", async () => {
    search = "?bot=steward";
    renderWizard();
    const card = await screen.findByRole("radio", { name: /Probe/ });
    fireEvent.click(card);
    expect(setLocation).toHaveBeenCalledWith(
      "/sources/plug?bot=steward&source=probe&step=account",
      { replace: true },
    );
  });
});

describe("PlugWizardView — local mode", () => {
  it("answers the account step honestly instead of spinning on a cloud query", async () => {
    vi.mocked(useServerInfoStoreMock).mockReturnValue("local");
    search = "?source=probe&step=account";
    renderWizard();
    await screen.findByText(/iterion connections/);
    expect(listConnections).not.toHaveBeenCalled();
  });
});

describe("PlugWizardView — the enable's terminal state", () => {
  it("reopens DONE from the URL and never re-offers the enable gesture", async () => {
    search = "?source=probe&step=review&connection=conn-1&trigger=probe.issue.commented&bot=steward&enabled=1";
    renderWizard();
    await screen.findByText(/Abonnement activé/);
    expect(screen.queryByRole("button", { name: /Activer/ })).toBeNull();
    // The vendor-side instructions the operator follows next.
    expect(screen.getByText("Add the hook at the vendor.")).toBeTruthy();
    expect(createTrigger).not.toHaveBeenCalled();
  });
});

describe("PlugWizardView — a polled trigger", () => {
  it("says each trigger's delivery on a mixed card and stays on the event step for a polled one", async () => {
    search = "?source=mix&step=event&connection=conn-1";
    renderWizard();
    const polled = await screen.findByRole("radio", { name: /Issue changed \(polled\)/ });
    expect(polled.textContent).toContain("interrogé");
    expect(screen.getByRole("radio", { name: /Issue updated/ }).textContent).toContain("webhook");
    fireEvent.click(polled);
    // The query and the contract are on this step: picking does not skip it.
    expect(setLocation).toHaveBeenCalledWith(
      "/sources/plug?source=mix&step=event&connection=conn-1&trigger=mix.issue.polled",
      { replace: true },
    );
  });

  it("states the contract and renders one typed input per query field, holding Continuer on the required one", async () => {
    search = "?source=mix&step=event&connection=conn-1&trigger=mix.issue.polled";
    renderWizard();
    await screen.findByText(
      "Interrogé toutes les 5 min : voit les versions qu'il observe à chaque lecture, pas " +
        "l'historique entre deux lectures. L'activer ne rejoue pas l'existant ; un backfill " +
        "demande explicitement les derniers éléments.",
    );
    const jql = screen.getByLabelText("Requête jql (requis)") as HTMLInputElement;
    expect(jql.tagName).toBe("INPUT");
    expect(jql.type).toBe("text");
    expect((screen.getByLabelText("Requête limit") as HTMLInputElement).type).toBe("number");
    const archived = screen.getByLabelText("Requête archived") as HTMLSelectElement;
    expect(archived.tagName).toBe("SELECT");
    expect(Array.from(archived.options).map((o) => o.value)).toEqual(["", "true", "false"]);
    const next = screen.getByRole("button", { name: "Continuer" }) as HTMLButtonElement;
    expect(next.disabled).toBe(true);
    fireEvent.change(jql, { target: { value: "project = OPS" } });
    expect(next.disabled).toBe(false);
  });

  it("sends the query to the preview and the subscription, and mints no webhook", async () => {
    search = "?source=mix&step=event&connection=conn-1&trigger=mix.issue.polled&bot=steward";
    const { goto } = renderWizard();
    fireEvent.change(await screen.findByLabelText("Requête jql (requis)"), {
      target: { value: "project = OPS" },
    });
    fireEvent.change(screen.getByLabelText("Requête limit"), { target: { value: "20" } });
    fireEvent.change(screen.getByLabelText("Requête archived"), { target: { value: "true" } });
    const query = { jql: "project = OPS", limit: "20", archived: "true" };

    goto("?source=mix&step=mapping&connection=conn-1&trigger=mix.issue.polled&bot=steward");
    fireEvent.click(await screen.findByRole("button", { name: /Prévisualiser/ }));
    await waitFor(() => screen.getByText(/premier élément de l'échantillon/));
    expect(previewTrigger).toHaveBeenCalledWith(
      expect.objectContaining({ trigger_id: "mix.issue.polled", poll_query: query }),
    );

    goto("?source=mix&step=review&connection=conn-1&trigger=mix.issue.polled&bot=steward");
    fireEvent.click(await screen.findByRole("button", { name: /Activer/ }));
    await waitFor(() => expect(createTrigger).toHaveBeenCalled());
    expect(createTrigger).toHaveBeenCalledWith(
      expect.objectContaining({
        invocation: "connector",
        connection_id: "conn-1",
        trigger_id: "mix.issue.polled",
        poll_query: query,
        backfill: 0,
      }),
    );
    expect(listWebhooks).not.toHaveBeenCalled();
    expect(createWebhook).not.toHaveBeenCalled();
  });

  it("sends the backfill typed at activation, and refuses one that is not a whole number", async () => {
    search = "?source=feed&step=review&connection=conn-1&trigger=feed.item.published&bot=steward";
    renderWizard();
    const backfill = await screen.findByLabelText("Backfill");
    const enable = screen.getByRole("button", { name: /Activer/ }) as HTMLButtonElement;
    fireEvent.change(backfill, { target: { value: "-1" } });
    expect(enable.disabled).toBe(true);
    fireEvent.change(backfill, { target: { value: "2" } });
    expect(enable.disabled).toBe(false);
    fireEvent.click(enable);
    await waitFor(() => expect(createTrigger).toHaveBeenCalled());
    expect(createTrigger).toHaveBeenCalledWith(
      expect.objectContaining({ trigger_id: "feed.item.published", backfill: 2 }),
    );
    expect(createWebhook).not.toHaveBeenCalled();
  });

  it("says nothing of a webhook, a delivery URL or a secret, before or after enabling", async () => {
    search = "?source=feed&step=review&connection=conn-1&trigger=feed.item.published&bot=steward";
    const { container } = renderWizard();
    fireEvent.click(await screen.findByRole("button", { name: /Activer/ }));
    await waitFor(() => expect(createTrigger).toHaveBeenCalled());
    await waitFor(() => expect(setLocation).toHaveBeenCalled());
    expect(container.textContent).not.toMatch(/webhook|secret|URL à déclarer|chez le fournisseur/i);

    cleanup();
    search = "?source=feed&step=review&connection=conn-1&trigger=feed.item.published&bot=steward&enabled=1";
    const done = renderWizard();
    await screen.findByText(/Abonnement activé — iterion interroge Feed toutes les 15 min/);
    expect(done.container.textContent).not.toMatch(/webhook|secret|URL à déclarer|chez le fournisseur/i);
  });

  it("holds the enable until the connection grants action, and grants it in one gesture", async () => {
    listConnections.mockImplementation(async (_team: string, connector: string) => [
      { id: "conn-1", alias: "main", connector, capabilities: ["trigger"] },
    ]);
    search = "?source=feed&step=review&connection=conn-1&trigger=feed.item.published&bot=steward";
    renderWizard();
    const grant = await screen.findByRole("button", { name: "Accorder action" });
    expect((screen.getByRole("button", { name: /Activer/ }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.click(grant);
    await waitFor(() => expect(patchConnection).toHaveBeenCalled());
    expect(patchConnection).toHaveBeenCalledWith("t1", "conn-1", {
      capabilities: ["trigger", "action"],
    });
  });
});

describe("PlugWizardView — the setup notes", () => {
  it("shows the package's polling notes for a polled trigger, not its webhook steps", async () => {
    search = "?source=mix&step=review&connection=conn-1&trigger=mix.issue.polled&bot=steward";
    const { container } = renderWizard();
    await screen.findByText("Notes de configuration (Mix)");
    expect(screen.getByText(MIX_SETUP_POLL)).toBeTruthy();
    expect(container.textContent).not.toContain(MIX_SETUP);
  });

  it("shows setup.md for a webhook trigger of the same package", async () => {
    search = "?source=mix&step=review&connection=conn-1&trigger=mix.issue.updated&bot=steward";
    const { container } = renderWizard();
    await screen.findByText("Configuration chez le fournisseur (Mix)");
    expect(screen.getByText(MIX_SETUP)).toBeTruthy();
    expect(container.textContent).not.toContain(MIX_SETUP_POLL);
  });

  it("shows setup.md for a polled trigger whose package ships no polling notes", async () => {
    search = "?source=feed&step=review&connection=conn-1&trigger=feed.item.published&bot=steward";
    renderWizard();
    await screen.findByText("Notes de configuration (Feed)");
    expect(screen.getByText(FEED_SETUP)).toBeTruthy();
  });
});

describe("PlugWizardView — a list filter", () => {
  it("takes several values, each one a chip, and sends the ones kept", async () => {
    search = "?source=feed&step=event&connection=conn-1&trigger=feed.item.published&bot=steward";
    const { goto } = renderWizard();
    const input = (await screen.findByLabelText("Filtre category")) as HTMLInputElement;
    for (const value of ["sources", "Sources", "Pollers, weekly"]) {
      fireEvent.change(input, { target: { value } });
      fireEvent.keyDown(input, { key: "Enter" });
    }
    // A value is compared as written, as the server compares it: a case
    // apart is another value, and a comma stays inside it.
    expect(screen.getByRole("button", { name: "Remove Pollers, weekly" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Remove sources" }));
    expect(screen.getByText(/au moins une de ces valeurs ; vide, tout événement correspond/)).toBeTruthy();

    goto("?source=feed&step=mapping&connection=conn-1&trigger=feed.item.published&bot=steward");
    fireEvent.click(await screen.findByRole("button", { name: /Prévisualiser/ }));
    await waitFor(() => expect(previewTrigger).toHaveBeenCalled());
    expect(previewTrigger).toHaveBeenCalledWith(
      expect.objectContaining({ filters: { category: ["Sources", "Pollers, weekly"] } }),
    );

    goto("?source=feed&step=event&connection=conn-1&trigger=feed.item.published&bot=steward");
    for (const name of ["Remove Pollers, weekly", "Remove Sources"]) {
      fireEvent.click(await screen.findByRole("button", { name }));
    }
    goto("?source=feed&step=review&connection=conn-1&trigger=feed.item.published&bot=steward");
    fireEvent.click(await screen.findByRole("button", { name: /Activer/ }));
    await waitFor(() => expect(createTrigger).toHaveBeenCalled());
    // Emptied is untouched: no narrowing is sent.
    expect(createTrigger).toHaveBeenCalledWith(expect.objectContaining({ filters: {} }));
  });
});

describe("PlugWizardView — the review names the connection", () => {
  it("shows the connection's alias and display name, its id only beside them", async () => {
    listConnections.mockImplementation(async (_team: string, connector: string) => [
      {
        id: "conn_01a1f83f",
        alias: "main",
        display_name: "Jira de production",
        connector,
        capabilities: ["action", "trigger"],
      },
    ]);
    search =
      "?source=probe&step=review&connection=conn_01a1f83f&trigger=probe.issue.commented&bot=steward";
    renderWizard();
    const line = (await screen.findByText(/^Connexion :/)).closest("li");
    await waitFor(() =>
      expect(line?.textContent).toBe("Connexion : main — Jira de production (conn_01a1f83f)"),
    );
  });
});

describe("PlugWizardView — the setup notes are Markdown", () => {
  it("renders the package's headings and emphasis as elements, never as raw markup or HTML", async () => {
    listSources.mockResolvedValue([
      {
        ...MIXED,
        setup_poll:
          "# Polling a Jira site\n\n**Issue changed (polled)** reads the site <img src=x onerror=alert(1)> on an interval.\n",
      },
    ]);
    search = "?source=mix&step=review&connection=conn-1&trigger=mix.issue.polled&bot=steward";
    const { container } = renderWizard();
    const notes = (await screen.findByText("Notes de configuration (Mix)")).closest("details");
    const heading = notes?.querySelector("h2, h3, h4");
    expect(heading?.textContent).toBe("Polling a Jira site");
    expect(screen.getByText("Issue changed (polled)").tagName).toBe("STRONG");
    expect(container.textContent).not.toContain("# Polling");
    expect(container.textContent).not.toContain("**");
    // The package's HTML is never rendered as an element.
    expect(notes?.querySelector("img")).toBeNull();
  });
});

describe("PlugWizardView — the preview's verdict, in French", () => {
  const goPreview = async () => {
    search = "?source=feed&step=mapping&connection=conn-1&trigger=feed.item.published&bot=steward";
    renderWizard();
    fireEvent.click(await screen.findByRole("button", { name: /Prévisualiser/ }));
  };

  it("says the sample does not pass the filters, and keeps the server's detail verbatim", async () => {
    const reason =
      'filter category: no element of ["sources","pollers"] is one of ["Security" "Open Source"]';
    previewTrigger.mockResolvedValue({ matched: false, sample: true, reason });
    await goPreview();
    await screen.findByText(
      "Cet événement ne passe pas vos filtres : l'abonnement ne lancerait pas le bot.",
    );
    expect(screen.getByText(reason).tagName).toBe("CODE");
    expect(screen.getByText(/L'échantillon est un exemple fourni par le paquet/)).toBeTruthy();
    expect(screen.queryByText(/^Refusé/)).toBeNull();
  });

  it("says a refusal is final, with the server's detail", async () => {
    const refusal = 'var_map title (event.missing): unknown field "missing"';
    previewTrigger.mockResolvedValue({ matched: false, refusal });
    await goPreview();
    await screen.findByText(/^L'abonnement refuserait cet événement/);
    expect(screen.getByText(refusal).tagName).toBe("CODE");
  });

  it("says the trigger's loop guard dropped it, with the server's detail", async () => {
    const filtered = "trigger feed.item.published's loop guard dropped it";
    previewTrigger.mockResolvedValue({ matched: false, filtered });
    await goPreview();
    await screen.findByText(/^Écarté par le déclencheur avant vos filtres/);
    expect(screen.getByText(filtered).tagName).toBe("CODE");
  });
});
