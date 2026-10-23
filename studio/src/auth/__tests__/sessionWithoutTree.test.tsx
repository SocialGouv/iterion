// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";

import { AuthProvider, useAuth } from "@/auth/AuthContext";

// The real AuthProvider against the server's contract, with only the HTTP
// boundary replaced: a route that mints a credential (login, refresh,
// switch) answers the session ALONE — never the org tree — and GET
// /api/auth/me is the one answer that carries it, failing with 500 when the
// server could not read it. The studio must never turn a missing tree into
// "you belong to nothing" (the restricted shell), nor a 500 into "signed
// out".

const ALICE = { id: "u-alice", email: "alice@example.test", status: "active", is_super_admin: false };
const TREE = [
  {
    org_id: "o1", org_name: "Org one", org_slug: "org-one", org_role: "admin",
    teams: [
      { team_id: "ta", team_name: "TA", team_slug: "ta", role: "member" },
      { team_id: "tb", team_name: "TB", team_slug: "tb", role: "admin" },
    ],
  },
];
// A session answer: the account and the active context, no tree.
const session = (team = "ta") => ({ user: ALICE, active_org_id: "o1", active_org_role: "admin", active_team_id: team, active_role: "member" });

type Answer = number | "network" | (() => Response);
let answers: Record<string, Answer[]>;
let calls: string[];

// answer queues the statuses a path answers in turn; the last one repeats.
function answer(path: string, ...seq: Answer[]) {
  answers[path] = seq;
}

function Probe() {
  const { status, user, orgs, activeTeamID, activeTeam, isRestricted, signIn, selectTeam, completeSignIn } = useAuth();
  return (
    <div>
      <span data-testid="state">
        {status}|{user?.id ?? "-"}|orgs={orgs.length}|team={activeTeamID || "-"}|teamInTree={String(Boolean(activeTeam))}|restricted={String(isRestricted)}
      </span>
      <button onClick={() => void signIn("alice@example.test", "pw")}>sign in</button>
      <button onClick={() => void selectTeam("tb").catch(() => undefined)}>switch</button>
      <button onClick={() => void completeSignIn()}>complete</button>
    </div>
  );
}

beforeEach(() => {
  answers = {};
  calls = [];
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const path = String(input).split("?")[0] ?? "";
    calls.push(`${init?.method ?? "GET"} ${path}`);
    const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
    if (path === "/api/server/info") return json({ auth_required: true, mode: "cloud" });
    const seq = answers[path];
    if (!seq || seq.length === 0) throw new Error(`Unexpected request: ${path}`);
    const next = (seq.length > 1 ? seq.shift() : seq[0]) as Answer;
    if (next === "network") throw new TypeError("Failed to fetch");
    if (typeof next === "function") return next();
    if (next >= 500) return json({ error: "identity unavailable" }, next);
    if (next >= 400) return json({ error: "refused" }, next);
    if (path === "/api/auth/me") return json({ user: ALICE, orgs: TREE, active_org_id: "o1", active_org_role: "admin", active_team_id: "ta", active_role: "member" });
    if (path === "/api/auth/me/team/tb") return json(session("tb"));
    return json(session());
  }));
});

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

const state = () => screen.getByTestId("state").textContent;
const settled = (re: RegExp) => screen.findByText(re, {}, { timeout: 6000 });

describe("bootstrap", () => {
  it("shows a /auth/me 500 as identity unavailable — not signed out", async () => {
    answer("/api/auth/me", 500);
    render(<AuthProvider><Probe /></AuthProvider>);
    await settled(/^identity_unavailable\|/);
    expect(calls).not.toContain("POST /api/auth/logout");
    expect(calls).not.toContain("POST /api/auth/refresh");
  }, 10000);

  it("renews an expired access token, then reads the tree from /auth/me — never from the refresh", async () => {
    answer("/api/auth/me", 401, 200);
    answer("/api/auth/refresh", 200);
    render(<AuthProvider><Probe /></AuthProvider>);
    await settled(/^authenticated\|/);
    expect(state()).toBe("authenticated|u-alice|orgs=1|team=ta|teamInTree=true|restricted=false");
    expect(calls).toEqual(["GET /api/server/info", "GET /api/auth/me", "POST /api/auth/refresh", "GET /api/auth/me"]);
  });

  it("shows a renewal the server could not serve as identity unavailable — not signed out", async () => {
    answer("/api/auth/me", 401);
    answer("/api/auth/refresh", 500);
    render(<AuthProvider><Probe /></AuthProvider>);
    await settled(/^identity_unavailable\|/);
    expect(calls).not.toContain("POST /api/auth/logout");
  });

  it("signs out on a renewal the server refuses", async () => {
    answer("/api/auth/me", 401);
    answer("/api/auth/refresh", 401);
    render(<AuthProvider><Probe /></AuthProvider>);
    await settled(/^anonymous\|/);
  });
});

describe("sign-in", () => {
  it("adopts the tree /auth/me answers after the login, which carries none", async () => {
    answer("/api/auth/me", 401, 200);
    answer("/api/auth/refresh", 401);
    answer("/api/auth/login", 200);
    render(<AuthProvider><Probe /></AuthProvider>);
    await settled(/^anonymous\|/);
    await act(async () => { screen.getByText("sign in").click(); });
    await settled(/^authenticated\|/);
    expect(state()).toBe("authenticated|u-alice|orgs=1|team=ta|teamInTree=true|restricted=false");
  });

  it("never shows an account in no org when /auth/me cannot answer after the login", async () => {
    answer("/api/auth/me", 401, 500);
    answer("/api/auth/refresh", 401);
    answer("/api/auth/login", 200);
    render(<AuthProvider><Probe /></AuthProvider>);
    await settled(/^anonymous\|/);
    await act(async () => { screen.getByText("sign in").click(); });
    await settled(/^identity_unavailable\|/);
    expect(state()).toContain("restricted=false");
  }, 10000);
});

describe("a credential flow completed outside the shell (reset, forced change)", () => {
  it("shows /auth/me failing as identity unavailable, not as the flow's failure", async () => {
    answer("/api/auth/me", 401, 500);
    answer("/api/auth/refresh", 401);
    render(<AuthProvider><Probe /></AuthProvider>);
    await settled(/^anonymous\|/);
    await act(async () => { screen.getByText("complete").click(); });
    await settled(/^identity_unavailable\|/);
  }, 10000);
});

// me answers /api/auth/me as a server does once a switch moved the session to
// team: the tree it reads now, and the active context of the token.
const me = (tree: typeof TREE, team: string) => () =>
  new Response(JSON.stringify({ user: ALICE, orgs: tree, active_org_id: "o1", active_org_role: "admin", active_team_id: team, active_role: "member" }), { status: 200 });

describe("a team switch", () => {
  it("moves the state to the context the switch answered at once, then re-reads the tree", async () => {
    answer("/api/auth/me", 200, me(TREE, "tb"));
    answer("/api/auth/me/team/tb", 200);
    render(<AuthProvider><Probe /></AuthProvider>);
    await settled(/^authenticated\|/);
    await act(async () => { screen.getByText("switch").click(); });
    await settled(/team=tb/);
    await vi.waitFor(() => expect(calls.filter((c) => c === "GET /api/auth/me")).toHaveLength(2));
    expect(state()).toBe("authenticated|u-alice|orgs=1|team=tb|teamInTree=true|restricted=false");
    expect(calls.indexOf("POST /api/auth/me/team/tb")).toBeLessThan(calls.lastIndexOf("GET /api/auth/me"));
  });

  it("adopts a tree that changed since it was read: a team granted since is in it", async () => {
    // tb was granted after the tree the state holds was read: the held tree
    // does not list it, the re-read after the switch does.
    const held = [{ org_id: "o1", org_name: "Org one", org_slug: "org-one", org_role: "admin", teams: [{ team_id: "ta", team_name: "TA", team_slug: "ta", role: "member" }] }];
    answer("/api/auth/me", () => me(held, "ta")(), me(TREE, "tb"));
    answer("/api/auth/me/team/tb", 200);
    render(<AuthProvider><Probe /></AuthProvider>);
    await settled(/^authenticated\|/);
    expect(state()).toContain("team=ta|teamInTree=true");
    await act(async () => { screen.getByText("switch").click(); });
    await settled(/team=tb\|teamInTree=true/);
  });

  it("keeps the switch and the tree it holds when the re-read fails", async () => {
    answer("/api/auth/me", 200, 500);
    answer("/api/auth/me/team/tb", 200);
    render(<AuthProvider><Probe /></AuthProvider>);
    await settled(/^authenticated\|/);
    await act(async () => { screen.getByText("switch").click(); });
    await settled(/team=tb/);
    await vi.waitFor(() => expect(calls.filter((c) => c === "GET /api/auth/me").length).toBeGreaterThanOrEqual(2), { timeout: 6000 });
    expect(state()).toBe("authenticated|u-alice|orgs=1|team=tb|teamInTree=true|restricted=false");
  }, 10000);

  it("leaves the state on its context when the switch fails", async () => {
    answer("/api/auth/me", 200);
    answer("/api/auth/me/team/tb", 500);
    render(<AuthProvider><Probe /></AuthProvider>);
    await settled(/^authenticated\|/);
    await act(async () => { screen.getByText("switch").click(); });
    expect(state()).toBe("authenticated|u-alice|orgs=1|team=ta|teamInTree=true|restricted=false");
  });
});
