// @vitest-environment jsdom
// Identity reads of the SAME session race each other, and the last answer is
// not the latest news: the server builds each answer when it PROCESSES the
// request, so a read sent earlier describes an earlier tree however late it
// lands. Every /auth/me send is numbered, and an answer from a send older than
// the one the state holds is dropped.
//
// Without that, the org a step just created vanishes from the switcher — and
// treeStale is cleared with it, so nothing says the view is behind and nothing
// re-reads. The identity GENERATION cannot stand in for it: it drops reads of
// an identity the browser has LEFT (a sign-out, a switch, a session a
// credential route delivered), while these are reads of the same identity.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen, waitFor } from "@testing-library/react";

import { AuthProvider, useAuth } from "@/auth/AuthContext";
import { cancelScheduledRefresh } from "@/api/client";

const USER = { id: "u1", email: "u1@example.test", status: "active", is_super_admin: false };
const O1 = { org_id: "o1", org_name: "O1", org_slug: "o1", org_role: "member", teams: [{ team_id: "ta", team_name: "TA", team_slug: "ta", role: "member" }] };
const O2 = { org_id: "o2", org_name: "O2", org_slug: "o2", org_role: "owner", teams: [] };
const O3 = { org_id: "o3", org_name: "O3", org_slug: "o3", org_role: "owner", teams: [] };

let serverTree: (typeof O1)[];
// script is what the server does with each /auth/me, in order: answer now,
// fail, or hold the answer until the test releases it.
let script: ("ok" | "500" | "hold")[];
// plan overrides script for one numbered send, so a test can place a failure
// on a read's own retry — whose position in TIME it controls by waiting on the
// send count rather than on a delay.
let plan: Map<number, "ok" | "500" | "hold">;
let held: (() => void)[];
let meSent: number;

function Probe() {
  const a = useAuth();
  return (
    <div>
      <span data-testid="state">{a.status}|orgs={a.orgs.map((o) => o.org_id).join(",")}|stale={String(a.treeStale)}</span>
      <button onClick={() => void a.reloadIdentity()}>reload</button>
    </div>
  );
}

beforeEach(() => {
  localStorage.clear();
  serverTree = [O1];
  script = [];
  plan = new Map();
  held = [];
  meSent = 0;
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request) => {
    const path = String(input).split("?")[0] ?? "";
    const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
    if (path === "/api/server/info") return json({ auth_required: true, mode: "cloud" });
    if (path === "/api/auth/me") {
      meSent++;
      const mode = plan.get(meSent) ?? script.shift() ?? "ok";
      // The tree as of NOW: the answer is built when the request is processed.
      const body = { user: USER, orgs: [...serverTree], active_org_id: "o1", active_team_id: "ta", active_role: "member", session_lineage: "L0" };
      if (mode === "500") return json({ error: "identity unavailable" }, 500);
      if (mode === "hold") return new Promise<Response>((resolve) => { held.push(() => resolve(json(body))); });
      return json(body);
    }
    throw new Error(`unexpected ${path}`);
  }));
});

afterEach(() => {
  for (const h of held.splice(0)) h();
  cancelScheduledRefresh();
  cleanup();
  localStorage.clear();
  vi.unstubAllGlobals();
});

const state = () => screen.getByTestId("state").textContent;

describe("an identity answer from a send older than the one the state holds", () => {
  it("is dropped when the stale-tree loop's attempt in flight answers after a fresher read", async () => {
    render(<AuthProvider><Probe /></AuthProvider>);
    await screen.findByText("authenticated|orgs=o1|stale=false");

    // A committed step's re-read fails three times: the tree is marked out of
    // date and the loop is armed (first attempt in 3 s).
    script = ["500", "500", "500"];
    screen.getByText("reload").click();
    await screen.findByText("authenticated|orgs=o1|stale=true", {}, { timeout: 6000 });

    // The loop's attempt is sent and processed (tree: o1), its answer slow.
    script = ["hold"];
    await waitFor(() => expect(held.length).toBe(1), { timeout: 8000 });

    // A second step commits (org o2) and its own re-read answers at once.
    serverTree = [O1, O2];
    script = ["ok"];
    screen.getByText("reload").click();
    await screen.findByText("authenticated|orgs=o1,o2|stale=false");

    // The loop's older answer lands now. It describes the server BEFORE o2.
    await act(async () => { held.shift()!(); });
    await new Promise((r) => setTimeout(r, 300));
    console.log("[order/loop] after the loop's older answer landed:", state());
    expect(state()).toBe("authenticated|orgs=o1,o2|stale=false");

    // And nothing has to re-read to repair it: the state already holds the
    // latest answer, so the tree is not stale.
    const sent = meSent;
    await new Promise((r) => setTimeout(r, 4000));
    console.log("[order/loop] identity reads in the 4 s after:", meSent - sent, "| final:", state());
    expect(meSent).toBe(sent);
    expect(state()).toBe("authenticated|orgs=o1,o2|stale=false");
  }, 30000);

  // The guard is at the choke point every answer passes, not in the loop: two
  // plain re-reads racing need it just as much.
  it("is dropped when two re-reads race and the older one answers last", async () => {
    render(<AuthProvider><Probe /></AuthProvider>);
    await screen.findByText("authenticated|orgs=o1|stale=false");

    // The first re-read is sent and processed before o2 exists, and held.
    script = ["hold"];
    screen.getByText("reload").click();
    await waitFor(() => expect(held.length).toBe(1), { timeout: 8000 });

    // o2 is created; the second re-read answers at once.
    serverTree = [O1, O2];
    script = ["ok"];
    screen.getByText("reload").click();
    await screen.findByText("authenticated|orgs=o1,o2|stale=false");

    await act(async () => { held.shift()!(); });
    await new Promise((r) => setTimeout(r, 300));
    console.log("[order/race] after the first read's older answer landed:", state());
    expect(state()).toBe("authenticated|orgs=o1,o2|stale=false");
  }, 30000);

  // A read's own retries are separate sends, and the one that ANSWERS is the
  // one the number has to come from: a read whose first attempts hit a 5xx and
  // whose last attempt is sent after another read was adopted describes the
  // server LATER, and dropping it would lose the org created in between with
  // nothing scheduled to re-read it.
  it("is adopted when a read's own retry is sent after another read was adopted", async () => {
    render(<AuthProvider><Probe /></AuthProvider>);
    await screen.findByText("authenticated|orgs=o1|stale=false");

    // The first re-read (sends 2 and 3) hits a 5xx twice; its third attempt is
    // armed 1 s after the second answered.
    plan.set(2, "500");
    plan.set(3, "500");
    screen.getByText("reload").click();
    await waitFor(() => expect(meSent).toBe(3), { timeout: 6000 });

    // o2 is created and a second re-read (send 4) answers at once: adopted.
    serverTree = [O1, O2];
    screen.getByText("reload").click();
    await screen.findByText("authenticated|orgs=o1,o2|stale=false");

    // o3 is created, and the FIRST read's last attempt (send 5) is sent now —
    // after send 4 — and answers the tree as of now.
    serverTree = [O1, O2, O3];
    await screen.findByText("authenticated|orgs=o1,o2,o3|stale=false", {}, { timeout: 6000 });
    console.log("[order/retry] sends:", meSent, "| final:", state());
    expect(meSent).toBe(5);
  }, 30000);

  // The other direction: an answer from a LATER send is adopted, whatever
  // order the state was in. A guard that refused everything would pass the two
  // above and freeze the switcher.
  it("is adopted when it comes from a later send", async () => {
    render(<AuthProvider><Probe /></AuthProvider>);
    await screen.findByText("authenticated|orgs=o1|stale=false");
    serverTree = [O1, O2];
    screen.getByText("reload").click();
    await screen.findByText("authenticated|orgs=o1,o2|stale=false");
    console.log("[order/forward] after a later send:", state());
  }, 20000);
});
