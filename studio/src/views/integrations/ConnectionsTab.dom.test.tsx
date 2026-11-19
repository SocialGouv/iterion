// @vitest-environment jsdom
// The add-connection form follows the scheme's kind, the server's paste gate
// (sealConnectionCredential): a scheme of kind none takes no credential, HTTP
// basic a username and a password, every other kind a token — and a value
// the scheme does not take is a 400. These witnesses drive the real form and
// read the body it hands to createConnection.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

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
  fireEvent.click(await screen.findByRole("button", { name: "Add a connection" }));
  await waitFor(() => expect(listTeamSources).toHaveBeenCalledWith("t1"));
  fireEvent.change(screen.getByLabelText("Connector"), { target: { value: connector } });
}

function submit() {
  fireEvent.click(screen.getByRole("button", { name: "Connect" }));
}

describe("ConnectionsTab — the credential follows the scheme's kind", () => {
  it("asks no credential for a scheme of kind none, and sends none", async () => {
    await openForm("feed");
    const scheme = (await screen.findByLabelText(/Auth scheme/)) as HTMLSelectElement;
    expect(scheme.tagName).toBe("SELECT");
    // Two schemes: no credential field until one is chosen.
    expect(screen.queryByLabelText("Credential")).toBeNull();
    fireEvent.change(scheme, { target: { value: "none" } });
    expect(screen.queryByLabelText("Credential")).toBeNull();
    expect(screen.queryByLabelText("Username")).toBeNull();
    expect(screen.queryByLabelText("Password")).toBeNull();
    expect(screen.getByText(/presents no credential/)).toBeTruthy();
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
    fireEvent.change(await screen.findByLabelText(/Auth scheme/), { target: { value: "basic" } });
    expect(screen.queryByLabelText("Credential")).toBeNull();
    fireEvent.change(screen.getByLabelText("Username"), { target: { value: "reader" } });
    fireEvent.change(screen.getByLabelText("Password"), { target: { value: "s3cret-pass" } });
    submit();
    await waitFor(() => expect(createConnection).toHaveBeenCalled());
    const body = createConnection.mock.calls[0]?.[1] as Record<string, unknown>;
    expect(body).toMatchObject({ scheme_id: "basic", username: "reader", password: "s3cret-pass" });
    expect(Object.keys(body)).not.toContain("token");
  });

  it("asks the token for a token-shaped scheme, and sends no username or password", async () => {
    await openForm("tracker");
    // One declared scheme: it is the one, no choice to make.
    expect(((await screen.findByLabelText(/Auth scheme/)) as HTMLSelectElement).value).toBe("bearer");
    expect(screen.queryByLabelText("Username")).toBeNull();
    fireEvent.change(screen.getByLabelText("Credential"), { target: { value: "tok-123456" } });
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
    await screen.findByText(/presents no credential/);
    expect(screen.queryByLabelText("Credential")).toBeNull();
  });

  it("asks for action and trigger when the package declares a polled trigger", async () => {
    await openForm("feed");
    await screen.findByText(/grant both action and trigger/);
    fireEvent.click(screen.getByLabelText("trigger"));
    expect(screen.queryByText(/grant both action and trigger/)).toBeNull();
  });
});
