// @vitest-environment jsdom
// The add-connection form follows the scheme's kind, the server's paste gate
// (sealConnectionCredential): a scheme of kind none takes no credential, HTTP
// basic a username and a password, every other kind a token — and a value
// the scheme does not take is a 400. These witnesses drive the real form and
// read the body it hands to createConnection.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";

const createConnection = vi.fn();
const listConnections = vi.fn();
const listTeamSources = vi.fn();

// The credential shape helpers stay real: they are what is under test.
vi.mock("@/api/connections", async () => {
  const actual = await vi.importActual<typeof import("@/api/connections")>("@/api/connections");
  return {
    ...actual,
    createConnection: (...a: unknown[]) => createConnection(...a),
    listConnections: (...a: unknown[]) => listConnections(...a),
  };
});
vi.mock("@/api/sources", async () => {
  const actual = await vi.importActual<typeof import("@/api/sources")>("@/api/sources");
  return { ...actual, listTeamSources: (...a: unknown[]) => listTeamSources(...a) };
});

import ConnectionsTab from "./ConnectionsTab";
import type { Source } from "@/api/sources";

const FEED: Source = {
  kind: "connector",
  id: "feed",
  display_name: "Feed",
  delivery: "poll",
  connections: 0,
  subscriptions: 0,
  auth: [
    { id: "none", kind: "none", display_name: "No credential" },
    { id: "basic", kind: "basic", display_name: "Username and password" },
  ],
  triggers: [{ id: "feed.item.published", display_name: "Entry published", delivery: "poll" }],
};

const TRACKER: Source = {
  kind: "connector",
  id: "tracker",
  display_name: "Tracker",
  delivery: "push",
  connections: 0,
  subscriptions: 0,
  auth: [{ id: "bearer", kind: "bearer" }],
  triggers: [{ id: "tracker.issue.created", delivery: "push" }],
};

beforeEach(() => {
  vi.clearAllMocks();
  listConnections.mockResolvedValue([]);
  listTeamSources.mockResolvedValue([FEED, TRACKER]);
  createConnection.mockResolvedValue({ connector: "feed", alias: "main", warnings: [] });
});

afterEach(cleanup);

async function openForm(connector: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <ConnectionsTab teamID="t1" canManage />
    </QueryClientProvider>,
  );
  fireEvent.click(await screen.findByRole("button", { name: "Ajouter une connexion" }));
  await waitFor(() => expect(listTeamSources).toHaveBeenCalledWith("t1"));
  await waitFor(() =>
    expect((screen.getByLabelText("Connecteur") as HTMLSelectElement).disabled).toBe(false),
  );
  fireEvent.change(screen.getByLabelText("Connecteur"), { target: { value: connector } });
}

function submit() {
  fireEvent.click(screen.getByRole("button", { name: "Connecter" }));
}

describe("ConnectionsTab — the credential follows the scheme's kind", () => {
  it("asks no credential for a scheme of kind none, and sends none", async () => {
    await openForm("feed");
    const scheme = (await screen.findByLabelText(/Méthode d'authentification/)) as HTMLSelectElement;
    expect(scheme.tagName).toBe("SELECT");
    // Two schemes: no credential field until one is chosen.
    expect(screen.queryByLabelText("Jeton d'accès")).toBeNull();
    fireEvent.change(scheme, { target: { value: "none" } });
    expect(screen.queryByLabelText("Jeton d'accès")).toBeNull();
    expect(screen.queryByLabelText("Nom d'utilisateur")).toBeNull();
    expect(screen.queryByLabelText("Mot de passe")).toBeNull();
    expect(screen.getByText(/ne présente aucun secret/)).toBeTruthy();
    submit();
    await waitFor(() => expect(createConnection).toHaveBeenCalled());
    const body = createConnection.mock.calls[0]?.[1] as Record<string, unknown>;
    expect(body).toMatchObject({ connector: "feed", scheme_id: "none" });
    expect(Object.keys(body)).not.toContain("token");
    expect(Object.keys(body)).not.toContain("username");
    expect(Object.keys(body)).not.toContain("password");
  });

  it("asks a username and a password for HTTP basic, and sends no token", async () => {
    await openForm("feed");
    fireEvent.change(await screen.findByLabelText(/Méthode d'authentification/), { target: { value: "basic" } });
    expect(screen.queryByLabelText("Jeton d'accès")).toBeNull();
    fireEvent.change(screen.getByLabelText("Nom d'utilisateur"), { target: { value: "reader" } });
    fireEvent.change(screen.getByLabelText("Mot de passe"), { target: { value: "s3cret-pass" } });
    submit();
    await waitFor(() => expect(createConnection).toHaveBeenCalled());
    const body = createConnection.mock.calls[0]?.[1] as Record<string, unknown>;
    expect(body).toMatchObject({ scheme_id: "basic", username: "reader", password: "s3cret-pass" });
    expect(Object.keys(body)).not.toContain("token");
  });

  it("asks the token for a token-shaped scheme, and sends no username or password", async () => {
    await openForm("tracker");
    // One declared scheme: it is the one, no choice to make.
    expect(((await screen.findByLabelText(/Méthode d'authentification/)) as HTMLSelectElement).value).toBe("bearer");
    expect(screen.queryByLabelText("Nom d'utilisateur")).toBeNull();
    fireEvent.change(screen.getByLabelText("Jeton d'accès"), { target: { value: "tok-123456" } });
    submit();
    await waitFor(() => expect(createConnection).toHaveBeenCalled());
    const body = createConnection.mock.calls[0]?.[1] as Record<string, unknown>;
    expect(body).toMatchObject({ connector: "tracker", token: "tok-123456" });
    expect(Object.keys(body)).not.toContain("username");
    expect(Object.keys(body)).not.toContain("password");
  });

  // A package declaring one scheme leaves no choice: that scheme's kind
  // decides the fields before the operator touches the scheme input.
  it("follows the only declared scheme's kind without a choice", async () => {
    listTeamSources.mockResolvedValue([
      { ...FEED, id: "planet", auth: [{ id: "public", kind: "none" }] },
    ]);
    await openForm("planet");
    await screen.findByText(/ne présente aucun secret/);
    expect(screen.queryByLabelText("Jeton d'accès")).toBeNull();
  });

  it("asks for action and trigger when the package declares a polled trigger", async () => {
    await openForm("feed");
    await screen.findByText(/accordez donc action et trigger/);
    fireEvent.click(screen.getByLabelText("trigger"));
    expect(screen.queryByText(/accordez donc action et trigger/)).toBeNull();
  });
});

// The plug wizard sends the operator here to create the connection its account
// step lacks: the form opens on that connector, and the operator goes back to
// the wizard once the connection exists — within the studio's base, and only
// to an in-app path.
const WIZARD = "/sources/plug?source=feed&step=account&bot=steward";

function renderAt(search: string) {
  const path = `/studio/integrations?${search}`;
  const { hook, history } = memoryLocation({ path, record: true });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <Router base="/studio" hook={hook}>
        <ConnectionsTab teamID="t1" canManage />
      </Router>
    </QueryClientProvider>,
  );
  return { history: history as string[], path };
}

async function connectorSelect() {
  await waitFor(() =>
    expect((screen.getByLabelText("Connecteur") as HTMLSelectElement).disabled).toBe(false),
  );
  return screen.getByLabelText("Connecteur") as HTMLSelectElement;
}

describe("ConnectionsTab — reached from the plug wizard", () => {
  it("opens the form on the requested connector and returns to the wizard once created", async () => {
    const { history } = renderAt(
      `tab=connectors&connector=feed&returnTo=${encodeURIComponent(WIZARD)}`,
    );
    const select = await connectorSelect();
    expect(select.tagName).toBe("SELECT");
    expect(select.value).toBe("feed");
    // The catalogue's connectors, and a way to name one it does not list.
    expect(Array.from(select.options).map((o) => o.textContent)).toEqual([
      "Feed (feed)",
      "Tracker (tracker)",
      "Autre connecteur, par identifiant…",
    ]);
    expect(screen.getByRole("link", { name: /Revenir au branchement/ }).getAttribute("href")).toBe(
      `/studio${WIZARD}`,
    );
    fireEvent.change(screen.getByLabelText(/Méthode d'authentification/), {
      target: { value: "none" },
    });
    submit();
    await waitFor(() => expect(createConnection).toHaveBeenCalled());
    expect(createConnection.mock.calls[0]?.[1]).toMatchObject({ connector: "feed" });
    await waitFor(() => expect(history[history.length - 1]).toBe(`/studio${WIZARD}`));
  });

  it.each([
    ["a protocol-relative URL", "//evil.example/x"],
    ["an absolute URL", "https://evil.example/x"],
    ["a backslash a browser reads as a slash", "/\\evil.example"],
    ["a script URL", "javascript:alert(1)"],
  ])("never goes back to %s", async (_label, target) => {
    const { history, path } = renderAt(
      `tab=connectors&connector=feed&returnTo=${encodeURIComponent(target)}`,
    );
    await connectorSelect();
    expect(screen.queryByRole("link", { name: /Revenir/ })).toBeNull();
    fireEvent.change(screen.getByLabelText(/Méthode d'authentification/), {
      target: { value: "none" },
    });
    submit();
    await waitFor(() => expect(createConnection).toHaveBeenCalled());
    // Created, the form closes, and the location stays where it was.
    await waitFor(() => expect(screen.queryByRole("button", { name: "Connecter" })).toBeNull());
    expect(history).toEqual([path]);
  });

  it("starts on no connector when none is requested, and holds the submit until one is picked", async () => {
    renderAt("tab=connectors");
    fireEvent.click(await screen.findByRole("button", { name: "Ajouter une connexion" }));
    const select = await connectorSelect();
    expect(select.value).toBe("");
    expect((screen.getByRole("button", { name: "Connecter" }) as HTMLButtonElement).disabled).toBe(
      true,
    );
  });

  it("takes a connector the catalogue does not list by its id", async () => {
    renderAt("tab=connectors&connector=outbound");
    const select = await connectorSelect();
    expect(select.selectedOptions[0]?.textContent).toBe("Autre connecteur, par identifiant…");
    const id = screen.getByLabelText("Identifiant du connecteur") as HTMLInputElement;
    expect(id.value).toBe("outbound");
    fireEvent.change(screen.getByLabelText("Jeton d'accès"), { target: { value: "tok-123456" } });
    submit();
    await waitFor(() => expect(createConnection).toHaveBeenCalled());
    expect(createConnection.mock.calls[0]?.[1]).toMatchObject({ connector: "outbound" });
  });

  it("keeps the id field of a connector the catalogue does not list while it is retyped", async () => {
    renderAt("tab=connectors&connector=outbound");
    const select = await connectorSelect();
    const id = () => screen.getByLabelText("Identifiant du connecteur") as HTMLInputElement;
    expect(id().value).toBe("outbound");
    // Cleared, then typed through a catalogue id on the way to a longer one:
    // the field stays under the cursor and the select stays on « Autre ».
    for (const value of ["", "feed", "feedback"]) {
      fireEvent.change(id(), { target: { value } });
      expect(id().value).toBe(value);
      expect(select.selectedOptions[0]?.textContent).toBe("Autre connecteur, par identifiant…");
    }
  });

  it("says the catalogue did not load, and takes the connector's id typed", async () => {
    listTeamSources.mockRejectedValue(new Error("catalogue down"));
    renderAt("tab=connectors&connector=feed");
    await screen.findByText(/Le catalogue des connecteurs n'a pas pu être chargé \(catalogue down\)/);
    const field = screen.getByLabelText("Connecteur") as HTMLInputElement;
    expect(field.tagName).toBe("INPUT");
    expect(field.value).toBe("feed");
  });
});

describe("ConnectionsTab — worded in French", () => {
  it("names the table's columns, the status and the actions in French", async () => {
    listConnections.mockResolvedValue([
      {
        id: "conn-1",
        alias: "main",
        connector: "jira",
        base_url: "https://jira.example",
        capabilities: ["action", "trigger"],
        status: "revoked",
        created_at: "2026-11-20T10:00:00Z",
      },
    ]);
    renderAt("tab=connectors");
    await screen.findByText("main");
    expect(screen.getAllByRole("columnheader").map((h) => h.textContent)).toEqual([
      "Alias",
      "Connecteur",
      "Instance",
      "Usages autorisés",
      "État",
      "Créée le",
      "",
    ]);
    expect(screen.getByText("révoquée")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Supprimer" })).toBeTruthy();
  });

  it("labels the form's fields in French", async () => {
    renderAt("tab=connectors&connector=tracker");
    await connectorSelect();
    for (const label of ["Connecteur", "Alias", "URL de l'instance", "Nom affiché", "Jeton d'accès"]) {
      expect(screen.getByLabelText(label)).toBeTruthy();
    }
    expect(screen.getByLabelText("Alias").getAttribute("placeholder")).toBe("main (par défaut)");
    expect(screen.getByText("Peut servir à")).toBeTruthy();
  });
});
