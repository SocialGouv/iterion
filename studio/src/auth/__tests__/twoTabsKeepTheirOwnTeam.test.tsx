// @vitest-environment jsdom
// The #2662 fix, end to end: two tabs — two module graphs — over ONE cookie
// jar. Tab 1 adopts team ta; tab 2 switches the shared cookie to tc; tab 1's
// next tenant-scoped request must still scope to ta (what the tab displays
// is what it acts on) while riding the cookie another tab moved, and tab 2
// scopes to tc.
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";

const USER = { id: "u1", email: "u1@example.test", status: "active", is_super_admin: false };
const ORGS = [{ org_id: "o1", org_name: "O1", org_slug: "o1", org_role: "member", teams: [
  { team_id: "ta", team_name: "TA", team_slug: "ta", role: "member" },
  { team_id: "tb", team_name: "TB", team_slug: "tb", role: "member" },
  { team_id: "tc", team_name: "TC", team_slug: "tc", role: "member" },
] }];

interface Jar { refresh: string | null; access: string | null }
let jar: Jar;
let live: Set<string>;
let mint: number;
// every tenant-scoped launch: the header it presented and the access cookie that carried it
let launches: { team: string | null; access: string | null }[];
let opened: { cancelScheduledRefresh: () => void }[];

const teamOf = (access: string) => access.split(":")[1] ?? "";
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

function stubServer() {
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const path = String(input).split("?")[0] ?? "";
    const method = init?.method ?? "GET";
    const sent = { ...jar };
    const authed = !!sent.access && live.has(sent.access);
    if (path === "/api/server/info") return json({ auth_required: true, mode: "cloud" });
    if (path === "/api/auth/me") {
      if (!authed) return json({ error: "authentication required" }, 401);
      return json({ user: USER, orgs: ORGS, active_org_id: "o1", active_team_id: teamOf(sent.access!), active_role: "member" });
    }
    if (path.startsWith("/api/auth/me/team/")) {
      const team = path.split("/").pop()!;
      mint++;
      const minted = `A${mint}:${team}`;
      live.add(minted);
      jar = { ...jar, access: minted };
      return json({ user: USER, active_org_id: "o1", active_team_id: team, active_role: "member" });
    }
    if (path === "/api/probe-launch") {
      const headers = new Headers(init?.headers);
      launches.push({ team: headers.get("x-iterion-team"), access: sent.access });
      return json({ ok: true });
    }
    if (path === "/api/auth/refresh") return json({ error: "no refresh token" }, 401);
    throw new Error(`unexpected ${method} ${path}`);
  }));
}

// One tab = one module graph (client + AuthContext), the way a browser tab
// holds its own copies; refreshAcrossTabs.test.ts models the same for the
// client alone. Each graph keeps its own published team.
type ClientMod = typeof import("@/api/client");
type AuthMod = typeof import("@/auth/AuthContext");

async function openTab(): Promise<{ client: ClientMod; auth: AuthMod }> {
  vi.resetModules();
  const client = await import("@/api/client");
  const auth = await import("@/auth/AuthContext");
  opened.push(client);
  return { client, auth };
}

function Probe({ useAuth, tab, onLaunch }: { useAuth: AuthMod["useAuth"]; tab: string; onLaunch: () => void }) {
  const a = useAuth();
  return (
    <div>
      <span data-testid={`team-${tab}`}>{a.activeTeamID}</span>
      <button data-testid={`launch-${tab}`} onClick={onLaunch}>launch</button>
      <button data-testid={`switch-${tab}`} onClick={() => void a.selectTeam("tc")}>to tc</button>
    </div>
  );
}

beforeEach(() => {
  localStorage.clear();
  jar = { refresh: "T0", access: "A0:ta" };
  live = new Set(["A0:ta"]);
  mint = 0;
  launches = [];
  opened = [];
  stubServer();
});

afterEach(() => {
  cleanup();
  for (const client of opened) client.cancelScheduledRefresh();
  vi.resetModules();
  vi.unstubAllGlobals();
  localStorage.clear();
});

it("a switch in one tab does not re-scope another tab's requests", async () => {
  // Tab 1 boots while the shared cookie's active team is ta.
  const t1 = await openTab();
  render(
    <t1.auth.AuthProvider>
      <Probe useAuth={t1.auth.useAuth} tab="1" onLaunch={() => void t1.client.apiRequest("/api/probe-launch", { method: "POST" })} />
    </t1.auth.AuthProvider>,
  );
  await waitFor(() => expect(screen.getByTestId("team-1").textContent).toBe("ta"));

  // Tab 2 (a fresh module graph) boots on the same jar, then switches it to tc.
  const t2 = await openTab();
  render(
    <t2.auth.AuthProvider>
      <Probe useAuth={t2.auth.useAuth} tab="2" onLaunch={() => void t2.client.apiRequest("/api/probe-launch", { method: "POST" })} />
    </t2.auth.AuthProvider>,
  );
  await waitFor(() => expect(screen.getByTestId("team-2").textContent).toBe("ta"));
  await act(async () => {
    fireEvent.click(screen.getByTestId("switch-2"));
  });
  await waitFor(() => expect(screen.getByTestId("team-2").textContent).toBe("tc"));
  // Tab 1 never switched: it still displays, and acts on, ta.
  expect(screen.getByTestId("team-1").textContent).toBe("ta");

  // Tab 1 launches: the request rides the cookie tab 2 moved, but scopes to
  // what tab 1 displays.
  await act(async () => {
    fireEvent.click(screen.getByTestId("launch-1"));
  });
  await waitFor(() => expect(launches.length).toBe(1));
  expect(launches[0]?.team).toBe("ta");
  expect(launches[0]?.access).toBe("A1:tc");

  // Tab 2 launches: it scopes to its own choice.
  await act(async () => {
    fireEvent.click(screen.getByTestId("launch-2"));
  });
  await waitFor(() => expect(launches.length).toBe(2));
  expect(launches[1]?.team).toBe("tc");
  expect(launches[1]?.access).toBe("A1:tc");
});
