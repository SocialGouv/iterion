// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";

import { AuthProvider, useAuth } from "@/auth/AuthContext";
import { cancelScheduledRefresh } from "@/api/client";

// Every identity load — the bootstrap and its "Retry now", reloadIdentity
// after a committed step, establishIdentity after a credential route, the
// re-read a switch starts — can land after the state has moved on. The answer
// then describes an identity the browser has LEFT: adopting it puts a
// signed-out user back in "authenticated" with the cookies gone, or puts the
// UI on the team the token left, so the studio labels and acts on one team
// while the server scopes to the other.
//
// One generation counter guards them all. Guarding only the switch's own
// re-read leaves every other load adopting whatever answers, whenever.

const ALICE = { id: "u-alice", email: "alice@example.test", status: "active", is_super_admin: false };
const TREE = [
  {
    org_id: "o1", org_name: "Org one", org_slug: "org-one", org_role: "admin",
    teams: [
      { team_id: "ta", team_name: "TA", team_slug: "ta", role: "member" },
      { team_id: "tb", team_name: "TB", team_slug: "tb", role: "admin" },
      { team_id: "tc", team_name: "TC", team_slug: "tc", role: "viewer" },
    ],
  },
];
const meFor = (team: string) => ({ user: ALICE, orgs: TREE, active_org_id: "o1", active_org_role: "admin", active_team_id: team, active_role: "member" });

// held /auth/me answers, in the order they were asked; each resolves on demand.
let held: { team: string; resolve: (r: Response) => void }[];
let calls: string[];
let tokenTeam: string; // the active team the browser's access cookie carries
let cookie: boolean;
let meMode: "hold" | "now";

function Probe() {
  const a = useAuth();
  return (
    <div>
      <span data-testid="state">{a.status}|team={a.activeTeamID || "-"}</span>
      <button onClick={() => void a.selectTeam("tb").catch(() => undefined)}>to tb</button>
      <button onClick={() => void a.selectTeam("tc").catch(() => undefined)}>to tc</button>
      <button onClick={() => void a.reloadIdentity()}>reload</button>
      <button onClick={() => void a.retryConnection()}>retry</button>
      <button onClick={() => void a.signOut()}>sign out</button>
    </div>
  );
}

beforeEach(() => {
  held = [];
  calls = [];
  tokenTeam = "ta";
  cookie = true;
  meMode = "now";
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const path = String(input).split("?")[0] ?? "";
    const method = init?.method ?? "GET";
    calls.push(`${method} ${path} [token=${cookie ? tokenTeam : "none"}]`);
    const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
    if (path === "/api/server/info") return json({ auth_required: true, mode: "cloud" });
    if (path === "/api/auth/logout") { cookie = false; return new Response(null, { status: 204 }); }
    if (path === "/api/auth/refresh") return json({ error: "auth: session not found" }, 401);
    if (path.startsWith("/api/auth/me/team/")) {
      tokenTeam = path.split("/").pop()!;
      return json({ user: ALICE, active_org_id: "o1", active_org_role: "admin", active_team_id: tokenTeam, active_role: "member" });
    }
    if (path === "/api/auth/me") {
      if (!cookie) return json({ error: "authentication required" }, 401);
      const team = tokenTeam; // the answer reflects the token the request carried
      if (meMode === "now") return json(meFor(team));
      return new Promise<Response>((resolve) => { held.push({ team, resolve }); });
    }
    throw new Error(`unexpected ${path}`);
  }));
});

afterEach(() => { cancelScheduledRefresh(); cleanup(); vi.unstubAllGlobals(); });

const state = () => screen.getByTestId("state").textContent;
const click = async (name: string) => { await act(async () => { screen.getByText(name).click(); }); };
const release = async (i: number) => {
  const h = held[i]!;
  await act(async () => {
    h.resolve(new Response(JSON.stringify(meFor(h.team)), { status: 200 }));
    await new Promise((r) => setTimeout(r, 50));
  });
};

describe("identity reads that land after the state moved", () => {
  it("ends on C when two switches A->B->C have their re-reads answer C then B", async () => {
    render(<AuthProvider><Probe /></AuthProvider>);
    await screen.findByText(/^authenticated\|team=ta/);
    meMode = "hold";
    await click("to tb");
    await click("to tc");
    await vi.waitFor(() => expect(held).toHaveLength(2));
    await release(1); // C's re-read first
    await release(0); // B's re-read, late
    expect(state()).toBe("authenticated|team=tc");
  });

  it("ends on the switch's team when a reloadIdentity started before it answers after it", async () => {
    render(<AuthProvider><Probe /></AuthProvider>);
    await screen.findByText(/^authenticated\|team=ta/);
    meMode = "hold";
    await click("reload"); // e.g. after an org rename (OrgPage) — /me sent with the ta token
    await click("to tb");
    await vi.waitFor(() => expect(held).toHaveLength(2));
    await release(1); // the switch's re-read (tb)
    await release(0); // the reload's read (ta), late
    expect(tokenTeam).toBe("tb");
    expect(state()).toBe("authenticated|team=tb");
  });

  it("stays signed out when a sign-out lands while the bootstrap's read is in flight", async () => {
    render(<AuthProvider><Probe /></AuthProvider>);
    await screen.findByText(/^authenticated\|team=ta/);
    meMode = "hold";
    await click("retry"); // the auto-retry of the identity-unavailable screen (retryConnection = bootstrap)
    await vi.waitFor(() => expect(held).toHaveLength(1));
    await click("sign out");
    await screen.findByText(/^anonymous/);
    await release(0); // the server recovered: the /me sent before the sign-out answers 200
    await new Promise((r) => setTimeout(r, 100));
    expect(cookie).toBe(false);
    expect(state()).toMatch(/^anonymous/);
  });

  it("stays signed out when a sign-out lands while a reloadIdentity is in flight", async () => {
    render(<AuthProvider><Probe /></AuthProvider>);
    await screen.findByText(/^authenticated\|team=ta/);
    meMode = "hold";
    await click("reload");
    await vi.waitFor(() => expect(held).toHaveLength(1));
    await click("sign out");
    await screen.findByText(/^anonymous/);
    await release(0);
    await new Promise((r) => setTimeout(r, 100));
    expect(cookie).toBe(false);
    expect(state()).toMatch(/^anonymous/);
  });
});
