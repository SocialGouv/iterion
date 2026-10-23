// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";

import { AuthProvider, useAuth } from "@/auth/AuthContext";
import { cancelScheduledRefresh } from "@/api/client";
import OrgSwitcher from "@/components/shared/OrgSwitcher";

// A step the server COMMITTED is followed by a re-read of the tree: an org
// created, an invitation accepted, a team added, a rename. The step succeeded
// and the screen says so — that part is right, and a read that fails must not
// be reported as the step's failure. But keeping the old tree SILENTLY leaves
// the org just created absent from the switcher and the rename showing the old
// name, with nothing scheduled to fix it and nothing saying the view is
// behind. So the tree is marked (treeStale) and asked for again until one
// lands, which clears the mark.

const ALICE = { id: "u-alice", email: "alice@example.test", status: "active", is_super_admin: false };
const ONE_ORG = [{ org_id: "o1", org_name: "Org one", org_slug: "org-one", org_role: "admin", teams: [{ team_id: "ta", team_name: "TA", team_slug: "ta", role: "member" }] }];
const TWO_ORGS = [
  ...ONE_ORG,
  { org_id: "o2", org_name: "Org two", org_slug: "org-two", org_role: "owner", teams: [] },
];

let calls: string[];
let meFails: boolean;
let tree: typeof ONE_ORG;
// What reloadIdentity did with the failed read, from its CALLER's side: the
// five callers (AcceptInvitation, OrgPage team creation, CreateOrgForm,
// OrgDrawer rename, OrgDangerZone) await it and report their step as done,
// so it must resolve.
let reloadOutcome: "pending" | "resolved" | "rejected";

function Probe() {
  const a = useAuth();
  return (
    <div>
      <span data-testid="state">{a.status}|orgs={a.orgs.map((o) => o.org_id).join(",")}|stale={String(a.treeStale)}</span>
      <button
        onClick={() => {
          reloadOutcome = "pending";
          void a.reloadIdentity().then(
            () => { reloadOutcome = "resolved"; },
            () => { reloadOutcome = "rejected"; },
          );
        }}
      >
        reload
      </button>
      <button onClick={() => void a.selectTeam("ta").catch(() => undefined)}>switch</button>
    </div>
  );
}

beforeEach(() => {
  calls = [];
  meFails = false;
  reloadOutcome = "pending";
  tree = ONE_ORG;
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request) => {
    const path = String(input).split("?")[0] ?? "";
    const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
    if (path === "/api/server/info") return json({ auth_required: true, mode: "cloud" });
    if (path.startsWith("/api/auth/me/team/")) {
      calls.push("POST /api/auth/me/team/ta");
      return json({ user: ALICE, active_org_id: "o1", active_org_role: "admin", active_team_id: "ta", active_role: "member" });
    }
    if (path === "/api/auth/me") {
      calls.push(meFails ? "GET /api/auth/me [failing]" : "GET /api/auth/me");
      if (meFails) return json({ error: "identity unavailable" }, 500);
      return json({ user: ALICE, orgs: tree, active_org_id: "o1", active_org_role: "admin", active_team_id: "ta", active_role: "member" });
    }
    throw new Error(`unexpected ${path}`);
  }));
});

afterEach(() => { cancelScheduledRefresh(); cleanup(); vi.unstubAllGlobals(); });

const state = () => screen.getByTestId("state").textContent;
const failingReads = () => calls.filter((c) => c.includes("[failing]")).length;

describe("a tree re-read a committed step could not get", () => {
  it("is marked out of date and asked for again until one lands", async () => {
    render(<AuthProvider><Probe /><OrgSwitcher /></AuthProvider>);
    await screen.findByText(/^authenticated\|orgs=o1\|stale=false/);
    // The step committed server-side (a second org now exists), and the
    // re-read that follows it cannot be served.
    meFails = true;
    tree = TWO_ORGS;
    await act(async () => {
      screen.getByText("reload").click();
      await new Promise((r) => setTimeout(r, 2500));
    });
    // The step is NOT reported as failed: its caller's await resolved, the
    // state keeps what it holds, and it says it may be behind.
    expect(reloadOutcome).toBe("resolved");
    expect(state()).toBe("authenticated|orgs=o1|stale=true");
    // Visible in the shell where the org/team switcher lives, and in that
    // switcher's accessible name — the hint is a caption, not a screen.
    expect(screen.getByText(/Organizations may be out of date/)).toBeTruthy();
    expect(screen.getByRole("button", { name: /Organizations may be out of date/ })).toBeTruthy();
    const afterFailure = failingReads();
    expect(afterFailure).toBeGreaterThan(0);

    // The server serves again: a scheduled re-read lands on its own, with no
    // page reload and no further click.
    meFails = false;
    await screen.findByText(/^authenticated\|orgs=o1,o2\|stale=false/, {}, { timeout: 10000 });
    expect(screen.queryByText(/Organizations may be out of date/)).toBeNull();
    expect(screen.queryByRole("button", { name: /Organizations may be out of date/ })).toBeNull();
    expect(calls.filter((c) => c === "GET /api/auth/me").length).toBeGreaterThanOrEqual(2);
  }, 25000);

  it("stops asking once a tree landed", async () => {
    render(<AuthProvider><Probe /></AuthProvider>);
    await screen.findByText(/^authenticated\|orgs=o1\|stale=false/);
    meFails = true;
    await act(async () => {
      screen.getByText("reload").click();
      await new Promise((r) => setTimeout(r, 2500));
    });
    expect(state()).toContain("stale=true");
    meFails = false;
    await screen.findByText(/stale=false/, {}, { timeout: 10000 });
    const settled = calls.length;
    await new Promise((r) => setTimeout(r, 7000));
    expect(calls.length).toBe(settled);
  }, 30000);

  it("marks the tree and re-reads it when a switch's background read fails", async () => {
    render(<AuthProvider><Probe /></AuthProvider>);
    await screen.findByText(/^authenticated\|orgs=o1\|stale=false/);
    meFails = true;
    tree = TWO_ORGS;
    await act(async () => {
      screen.getByText("switch").click();
      await new Promise((r) => setTimeout(r, 2500));
    });
    // The switch itself stands on the tree it holds; that tree is marked.
    expect(state()).toBe("authenticated|orgs=o1|stale=true");
    expect(failingReads()).toBeGreaterThan(0);
    meFails = false;
    await screen.findByText(/^authenticated\|orgs=o1,o2\|stale=false/, {}, { timeout: 10000 });
  }, 25000);
});
