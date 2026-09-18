// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render, screen } from "@testing-library/react";

import { AuthProvider, useAuth } from "@/auth/AuthContext";
import { useActiveRepoStore } from "@/store/activeRepo";

const ALICE = { id: "u-alice", email: "alice@example.test", status: "active", is_super_admin: false };
const BOB = { id: "u-bob", email: "bob@example.test", status: "active", is_super_admin: false };

let whoami: typeof ALICE;

function Probe() {
  const { status, user, reloadIdentity } = useAuth();
  return (
    <div>
      <span data-testid="who">{status === "authenticated" ? user?.id : status}</span>
      <button onClick={() => void reloadIdentity()}>reload</button>
    </div>
  );
}

beforeEach(() => {
  whoami = ALICE;
  // The real shape of the store signOut resets: a storage key scoped to the
  // account, and the repo it selected.
  useActiveRepoStore.setState({
    storageKey: "iterion.activeRepo.u-alice.team-1",
    selection: { kind: "repo", key: "SocialGouv/secret-repo" },
  } as never);
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request) => {
    const path = String(input).split("?")[0] ?? "";
    const json = (b: unknown) => new Response(JSON.stringify(b), { status: 200 });
    if (path === "/api/server/info") return json({ auth_required: true, mode: "cloud" });
    if (path === "/api/auth/providers") return json({ signup_mode: "invite_only", providers: [] });
    if (["/api/auth/me", "/api/auth/refresh"].includes(path)) {
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
});
