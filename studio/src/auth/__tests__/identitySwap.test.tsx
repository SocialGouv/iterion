// @vitest-environment jsdom
import { StrictMode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";

import { AuthProvider, useAuth } from "@/auth/AuthContext";
import { useActiveRepoStore } from "@/store/activeRepo";

const ALICE = { id: "u-alice", email: "alice@example.test", status: "active", is_super_admin: false };
const BOB = { id: "u-bob", email: "bob@example.test", status: "active", is_super_admin: false };

let whoami: typeof ALICE;
let serverIsDown = false;

// The store's own reset, captured before any test wraps it, so the counter can
// call through instead of standing in for it.
const realReset = useActiveRepoStore.getState().reset;
let resetCalls = 0;

function Probe() {
  const { status, user, reloadIdentity, signIn, retryConnection } = useAuth();
  return (
    <div>
      <span data-testid="who">{status === "authenticated" ? user?.id : status}</span>
      <button onClick={() => void reloadIdentity()}>reload</button>
      <button onClick={() => void signIn("bob@example.test", "pw")}>sign in</button>
      <button onClick={() => void retryConnection()}>retry</button>
    </div>
  );
}

beforeEach(() => {
  whoami = ALICE;
  serverIsDown = false;
  resetCalls = 0;
  // The real shape of the store signOut resets: a storage key scoped to the
  // account, and the repo it selected.
  useActiveRepoStore.setState({
    storageKey: "iterion.activeRepo.u-alice.team-1",
    selection: { kind: "repo", key: "SocialGouv/secret-repo" },
    reset: () => {
      resetCalls++;
      realReset();
    },
  } as never);
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request) => {
    const path = String(input).split("?")[0] ?? "";
    const json = (b: unknown) => new Response(JSON.stringify(b), { status: 200 });
    if (path === "/api/server/info") {
      return serverIsDown
        ? new Response("down", { status: 503 })
        : json({ auth_required: true, mode: "cloud" });
    }
    if (path === "/api/auth/providers") return json({ signup_mode: "invite_only", providers: [] });
    if (["/api/auth/me", "/api/auth/refresh", "/api/auth/login"].includes(path)) {
      return json({ user: whoami, orgs: [], active_role: "owner" });
    }
    throw new Error(`Unexpected request: ${path}`);
  }));
});

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

describe("the browser's identity changing under a live session", () => {
  // signOut drops the repo-first context "so a different account on this
  // browser never inherits the previous user's active repo". Signing out used
  // to be the only way the identity could change — it is not any more: a
  // password-reset link opened WITH a session ends in fresh cookies for the
  // token's owner, and the view that follows calls reloadIdentity.
  //
  // The reset therefore has to key on the identity, not on the route that
  // changed it. A guard at ResetPassword's call site would need repeating at
  // ForcedPasswordChange's, and at the next one.
  it("drops the previous account's context when the account changes", async () => {
    render(<AuthProvider><Probe /></AuthProvider>);
    expect(await screen.findByText("u-alice")).toBeTruthy();
    expect(useActiveRepoStore.getState().selection.kind).toBe("repo");

    // The password-reset confirm handed the browser Bob's cookies.
    whoami = BOB;
    await act(async () => { screen.getByText("reload").click(); });

    expect(await screen.findByText("u-bob")).toBeTruthy();
    expect(useActiveRepoStore.getState().selection).toEqual({ kind: "none" });
    expect(useActiveRepoStore.getState().storageKey).toBeNull();
  });

  // The same call runs on an ordinary refresh, where nothing changed. Resetting
  // there would throw away the operator's own selection on every reload.
  it("leaves the context alone when the account is the same", async () => {
    render(<AuthProvider><Probe /></AuthProvider>);
    expect(await screen.findByText("u-alice")).toBeTruthy();

    await act(async () => { screen.getByText("reload").click(); });

    expect(screen.getByTestId("who").textContent).toBe("u-alice");
    expect(useActiveRepoStore.getState().selection.kind).toBe("repo");
  });

  // The comparison has to live in the callback body, not in the setState
  // updater: React requires updaters to be pure, StrictMode double-invokes
  // them in dev, and a concurrent render that is discarded is replayed. An
  // external-store write placed there fires once per invocation — and fires
  // during another component's render, which is the path React reports as
  // "Cannot update a component while rendering a different component".
  //
  // reset() is idempotent, so the observable damage is the count. That is
  // exactly what makes the placement invisible to a test that only asserts the
  // end state, and why this one counts.
  it("resets exactly once per identity change, under StrictMode's double render", async () => {
    render(<StrictMode><AuthProvider><Probe /></AuthProvider></StrictMode>);
    expect(await screen.findByText("u-alice")).toBeTruthy();
    // StrictMode runs the mount effect twice, so the bootstrap adopts Alice
    // twice. Adopting the SAME identity must never reset.
    expect(resetCalls).toBe(0);

    whoami = BOB;
    await act(async () => { screen.getByText("reload").click(); });

    expect(await screen.findByText("u-bob")).toBeTruthy();
    expect(resetCalls).toBe(1);
  });

  // A server blip drops the in-memory user to null without emptying the
  // stores, which still hold Alice's account-scoped context. Keying the reset
  // on the PREVIOUS STATE's user then finds null and concludes "no change" —
  // so the next account to sign in inherits the previous one's active repo,
  // through a door signOut's reset was written to close.
  //
  // The identity the stores are scoped to is not the identity the auth state
  // currently shows; it has to be remembered separately, and survive the lapse.
  it("resets when a different account signs in after the session lapsed", async () => {
    render(<AuthProvider><Probe /></AuthProvider>);
    expect(await screen.findByText("u-alice")).toBeTruthy();

    serverIsDown = true;
    await act(async () => { screen.getByText("retry").click(); });
    expect(await screen.findByText("unreachable")).toBeTruthy();
    // The lapse itself must not touch the operator's context: coming back as
    // the same account has to find the repo it left.
    expect(useActiveRepoStore.getState().selection.kind).toBe("repo");

    serverIsDown = false;
    whoami = BOB;
    await act(async () => { screen.getByText("sign in").click(); });

    expect(await screen.findByText("u-bob")).toBeTruthy();
    expect(useActiveRepoStore.getState().selection).toEqual({ kind: "none" });
    expect(useActiveRepoStore.getState().storageKey).toBeNull();
  });
});
