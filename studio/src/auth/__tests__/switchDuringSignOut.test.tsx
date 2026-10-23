// @vitest-environment jsdom
// A team/org switch mints a credential — the access cookie of its context —
// so a switch in flight when the user signs out must not sign them back in:
// the sign-out cancels it (an aborted request's answer never reaches the
// cookie jar), an answer that lands after the sign-out began anyway is
// dropped and the logout presented again (its Set-Cookie may have landed
// after the logout's clear), and no switch starts while a sign-out is pending.
//
// Real AuthProvider and client; the HTTP boundary keeps a cookie jar read
// when a request is sent and moved when its answer arrives. A switch mints a
// fresh access token from the one it is sent with; a logout clears the jar.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";

import { AuthProvider, useAuth } from "@/auth/AuthContext";
import { apiRequest, cancelScheduledRefresh, SIGN_OUT_PENDING_KEY } from "@/api/client";

const USER = { id: "u1", email: "u1@example.test", status: "active", is_super_admin: false };
const ORGS = [{ org_id: "o1", org_name: "O1", org_slug: "o1", org_role: "member", teams: [
  { team_id: "ta", team_name: "TA", team_slug: "ta", role: "member" },
  { team_id: "tb", team_name: "TB", team_slug: "tb", role: "member" },
  { team_id: "tc", team_name: "TC", team_slug: "tc", role: "member" },
] }];

interface Jar { refresh: string | null; access: string | null }
let jar: Jar;
let live: Set<string>;
let n: number;
let calls: string[];
// held switches, in the order they were sent; each answers on demand.
let switches: { team: string; release: () => void; aborted: boolean }[];
// A browser drops an aborted request's answer; "ignores" models a fetch that
// does not, so the late answer's cookie lands after the logout's clear.
let abortMode: "honoured" | "ignored";
let logoutHold: boolean;
let releaseLogout: (() => void) | null;

const teamOf = (access: string) => access.split(":")[1] ?? "";
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

function Probe() {
  const a = useAuth();
  return (
    <div>
      <span data-testid="state">{a.status}|team={a.activeTeamID}</span>
      <button onClick={() => void a.selectTeam("tb")}>to tb</button>
      <button onClick={() => void a.selectTeam("tc")}>to tc</button>
      <button onClick={() => void a.signOut()}>sign out</button>
    </div>
  );
}

beforeEach(() => {
  localStorage.clear();
  jar = { refresh: "T0", access: "A0:ta" };
  live = new Set(["A0:ta"]);
  n = 0;
  calls = [];
  switches = [];
  abortMode = "honoured";
  logoutHold = false;
  releaseLogout = null;
  loggedOut = false;
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const path = String(input).split("?")[0] ?? "";
    const method = init?.method ?? "GET";
    const sent = { ...jar };
    calls.push(`${method} ${path} [refresh=${sent.refresh ?? "none"} access=${sent.access ?? "none"}]`);
    const authed = !!sent.access && live.has(sent.access);
    if (path === "/api/server/info") return json({ auth_required: true, mode: "cloud" });
    if (path === "/api/auth/me") {
      if (!authed) return json({ error: "authentication required" }, 401);
      return json({ user: USER, orgs: ORGS, active_org_id: "o1", active_team_id: teamOf(sent.access!), active_role: "member" });
    }
    if (path.startsWith("/api/auth/me/team/")) {
      const team = path.split("/").pop()!;
      return new Promise<Response>((resolve, reject) => {
        const entry = { team, aborted: false, release: () => {} };
        entry.release = () => {
          if (entry.aborted) return;
          if (!authed) { resolve(json({ error: "authentication required" }, 401)); return; }
          n++;
          const minted = `A${n}:${team}`;
          live.add(minted);
          jar = { ...jar, access: minted };
          resolve(json({ user: USER, active_org_id: "o1", active_team_id: team, active_role: "member" }));
        };
        init?.signal?.addEventListener("abort", () => {
          if (abortMode !== "honoured") return;
          entry.aborted = true;
          reject(new DOMException("aborted", "AbortError"));
        });
        switches.push(entry);
      });
    }
    if (path === "/api/auth/logout") {
      const answer = () => {
        jar = { refresh: null, access: null };
        loggedOut = true;
        return new Response(null, { status: 204 });
      };
      if (!logoutHold) return answer();
      return new Promise<Response>((resolve) => { releaseLogout = () => resolve(answer()); });
    }
    if (path === "/api/auth/refresh") return json({ error: "no refresh token" }, 401);
    // The logout ends the session: an access cookie that lands after it (a
    // switch's Set-Cookie the abort could not recall) presents a revoked
    // session, and the server refuses it.
    if (path === "/api/probe") return authed && !loggedOut ? json({ ok: true }) : json({ error: "session revoked" }, 401);
    throw new Error(`unexpected ${method} ${path}`);
  }));
});

afterEach(() => {
  for (const s of switches) s.release();
  releaseLogout?.();
  cancelScheduledRefresh();
  cleanup();
  localStorage.clear();
  vi.unstubAllGlobals();
});

const state = () => screen.getByTestId("state").textContent;
const logouts = () => calls.filter((c) => c.startsWith("POST /api/auth/logout"));
let loggedOut = false;
const click = async (name: string) => { await act(async () => { screen.getByText(name).click(); }); };

describe("a switch in flight when the user signs out", () => {
  it("is cancelled: its answer never sets a cookie, and the browser stays signed out", async () => {
    render(<AuthProvider><Probe /></AuthProvider>);
    await screen.findByText("authenticated|team=ta");
    await click("to tb");
    await waitFor(() => expect(switches).toHaveLength(1));
    await click("sign out");
    await screen.findByText("anonymous|team=");
    expect(switches[0]!.aborted).toBe(true);
    // The server got to it anyway: its answer comes back, and is dropped.
    await act(async () => { switches[0]!.release(); await new Promise((r) => setTimeout(r, 100)); });
    expect(state()).toBe("anonymous|team=");
    expect(jar).toEqual({ refresh: null, access: null });
    expect(logouts()).toHaveLength(1);
  });

  it("is dropped when its answer lands after the logout, and nothing is presented again", async () => {
    abortMode = "ignored";
    render(<AuthProvider><Probe /></AuthProvider>);
    await screen.findByText("authenticated|team=ta");
    await click("to tb");
    await waitFor(() => expect(switches).toHaveLength(1));
    await click("sign out");
    await screen.findByText("anonymous|team=");
    // The switch answers after the logout cleared the jar: its access cookie
    // lands on a signed-out browser. The answer is DROPPED — and the machine
    // has no re-presented logout here: a switch is not a sign-out ask, and a
    // second unnamed logout would be about whatever session this browser
    // opens next (I3).
    await act(async () => { switches[0]!.release(); await new Promise((r) => setTimeout(r, 100)); });
    expect(state()).toBe("anonymous|team=");
    await act(async () => { await new Promise((r) => setTimeout(r, 300)); });
    expect(logouts()).toHaveLength(1);
    await expect(apiRequest("/api/probe")).rejects.toThrow();
  });

  it("is dropped when it answers while the logout is still on the wire, and that logout clears what it set", async () => {
    abortMode = "ignored";
    logoutHold = true;
    render(<AuthProvider><Probe /></AuthProvider>);
    await screen.findByText("authenticated|team=ta");
    await click("to tb");
    await waitFor(() => expect(switches).toHaveLength(1));
    await click("sign out");
    await waitFor(() => expect(releaseLogout).not.toBeNull());
    await act(async () => { switches[0]!.release(); await new Promise((r) => setTimeout(r, 100)); });
    expect(state()).toBe("signing_out|team=");
    await act(async () => { releaseLogout!(); releaseLogout = null; await new Promise((r) => setTimeout(r, 100)); });
    expect(state()).toBe("anonymous|team=");
    expect(jar).toEqual({ refresh: null, access: null });
    expect(logouts()).toHaveLength(1);
  });
});

describe("a switch while a sign-out is pending", () => {
  it("is dropped when it answers once another tab's sign-out is pending", async () => {
    abortMode = "ignored";
    render(<AuthProvider><Probe /></AuthProvider>);
    await screen.findByText("authenticated|team=ta");
    await click("to tb");
    await waitFor(() => expect(switches).toHaveLength(1));
    // Another tab's sign-out: this tab's storage event not delivered yet.
    localStorage.setItem(SIGN_OUT_PENDING_KEY, String(Date.now()));
    await act(async () => { switches[0]!.release(); await new Promise((r) => setTimeout(r, 100)); });
    expect(state()).toBe("authenticated|team=ta");
  });

  it("is not sent", async () => {
    render(<AuthProvider><Probe /></AuthProvider>);
    await screen.findByText("authenticated|team=ta");
    // Another tab's sign-out the server has not confirmed.
    localStorage.setItem(SIGN_OUT_PENDING_KEY, String(Date.now()));
    await click("to tb");
    await new Promise((r) => setTimeout(r, 50));
    expect(calls.filter((c) => c.startsWith("POST /api/auth/me/team/"))).toEqual([]);
    expect(state()).toBe("authenticated|team=ta");
  });
});

describe("two switches in flight", () => {
  it("end on the one that answered last, as the browser's cookie does", async () => {
    render(<AuthProvider><Probe /></AuthProvider>);
    await screen.findByText("authenticated|team=ta");
    await click("to tb");
    await click("to tc");
    await waitFor(() => expect(switches).toHaveLength(2));
    await act(async () => { switches[0]!.release(); await new Promise((r) => setTimeout(r, 50)); });
    await act(async () => { switches[1]!.release(); await new Promise((r) => setTimeout(r, 50)); });
    expect(teamOf(jar.access!)).toBe("tc");
    await waitFor(() => expect(state()).toBe("authenticated|team=tc"));
  });
});
