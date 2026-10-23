// @vitest-environment jsdom
// A password change the server COMMITTED but could not open a session for
// answers 409 password_changed_sign_in and clears the auth cookies: the
// account's password IS the new one, and this browser is signed out. The three
// forms that change a password have to say so with the server's own sentence,
// land the user on the sign-in form, and not retry — a second attempt carries
// the old password, which is no longer the account's, and would be answered
// "invalid credentials".
//
// A 409 credential_change_busy is the opposite and shares the status: nothing
// happened, the password is unchanged, and the user retries. The code decides,
// not the status.
import { Suspense, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

vi.mock("@/components/shared/AppShell", () => ({ default: ({ children }: { children: ReactNode }) => <Suspense fallback={null}>{children}</Suspense> }));
vi.mock("@/components/shared/GlobalCommandPalette", () => ({ default: () => null }));
vi.mock("@/components/shared/Toast", () => ({ default: () => null }));
vi.mock("@/views/SettingsDialog", () => ({ default: () => null }));
vi.mock("@/components/shared/CloudReloginModal", () => ({ default: () => null }));
vi.mock("@/components/ChatDock/ChatDock", () => ({ default: () => null, AssistantDockCrashed: () => null }));
vi.mock("@/components/Home/HomeView", () => ({ default: () => <h1>Signed-in home</h1> }));
vi.mock("@/views/CloudLanding", () => ({ default: () => <h1>Public cloud home</h1>, PublicTopBar: () => null }));
vi.mock("@/views/RestrictedShell", () => ({ default: () => null }));
vi.mock("@/views/ConfigShare", () => ({ default: () => null }));
vi.mock("@/views/Marketplace", () => ({ default: () => null }));
vi.mock("@/hooks/useProjectSwitchListener", () => ({ useProjectSwitchListener: () => undefined }));
vi.mock("@/hooks/useProjectScopeSync", () => ({ useProjectScopeSync: () => undefined }));
vi.mock("@/hooks/useDesktop", () => ({ useDesktop: () => ({ ready: true, isDesktop: false }) }));
vi.mock("@/components/Runs/RunsTabsView", () => ({ default: () => <h1>Requested run</h1> }));

import App from "@/App";
import { cancelScheduledRefresh } from "@/api/client";
import { useServerInfoStore } from "@/store/serverInfo";
import type { ServerInfo } from "@/api/types";

const account = {
  user: { id: "u", email: "r@example.test", status: "active", is_super_admin: false },
  orgs: [{ org_id: "o1", org_name: "O1", org_slug: "o1", org_role: "member", teams: [{ team_id: "t1", team_name: "T1", team_slug: "t1", role: "member" }] }],
  active_org_id: "o1", active_team_id: "t1", active_role: "member",
  session_lineage: "L0",
};

// The server's own sentences, as pkg/server answers them.
const CHANGED = "password changed, but no session could be opened: sign in with the new password";
const BUSY = "another password change of this account is in progress: try again in a moment";

type Answer = { status: number; body: unknown };
const CHANGED_409: Answer = { status: 409, body: { error: CHANGED, error_code: "password_changed_sign_in" } };
const BUSY_409: Answer = { status: 409, body: { error: BUSY, error_code: "credential_change_busy" } };

// signedIn = false models the two public doors (the emailed reset, the forced
// rotation), reached with no session.
let signedIn: boolean;
let answer: Answer;
let posts: string[];

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

function mount() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><App /></QueryClientProvider>);
}

beforeEach(() => {
  localStorage.clear();
  signedIn = true;
  answer = CHANGED_409;
  posts = [];
  useServerInfoStore.setState({ info: { mode: "cloud", auth_required: true } as ServerInfo });
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const path = String(input).split("?")[0] ?? "";
    const method = init?.method ?? "GET";
    if (method === "POST") posts.push(path);
    if (path === "/api/server/info") return json({ auth_required: true, mode: "cloud" });
    if (path === "/api/auth/providers") return json({ signup_mode: "invite_only", providers: [] });
    if (path === "/api/auth/me") return signedIn ? json(account) : json({ error: "authentication required" }, 401);
    if (path === "/api/me/sso/links") return json({ links: [] });
    if (path === "/api/auth/logout") return new Response(null, { status: 204 });
    if (path === "/api/me/password" || path === "/api/auth/password/reset/confirm" || path === "/api/auth/password/change") {
      // The cookies are cleared with the 409: the browser IS signed out.
      if (answer.status === 409 && answer === CHANGED_409) signedIn = false;
      return json(answer.body, answer.status);
    }
    if (path.startsWith("/api/")) return json({});
    throw new Error(`unexpected ${method} ${path}`);
  }));
});

afterEach(() => {
  cancelScheduledRefresh();
  cleanup();
  localStorage.clear();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

function type(label: string, value: string) {
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
}

// The account page's own form, reached where an operator reaches it: the
// Profile tab of /account, which ?sso_linked lands on.
async function openAccountPasswordForm() {
  window.history.replaceState({}, "", "/studio/account?sso_linked=github");
  mount();
  await screen.findByLabelText("Current password", {}, { timeout: 8000 });
}

async function expectSignInWithTheServersSentence(route: string) {
  await screen.findByRole("heading", { name: "Sign in to iterion" }, { timeout: 8000 });
  const notice = await screen.findByRole("alert", {}, { timeout: 8000 });
  console.log(`[409] ${route} →`, JSON.stringify(notice.textContent));
  expect(notice.textContent).toContain(CHANGED);
  // One attempt, no retry of a request whose credential the server has
  // already replaced.
  expect(posts.filter((p) => p === route)).toHaveLength(1);
}

describe("a password the server changed without opening a session", () => {
  it("sends the account form's user to sign in, with the server's sentence", async () => {
    await openAccountPasswordForm();
    type("Current password", "old-password");
    type("New password", "new-password-1");
    type("Confirm new password", "new-password-1");
    screen.getByRole("button", { name: "Change password" }).click();
    await expectSignInWithTheServersSentence("/api/me/password");
    expect(screen.queryByLabelText("Current password")).toBeNull();
  }, 30000);

  it("sends the emailed reset's user to sign in, with the server's sentence", async () => {
    signedIn = false;
    window.history.replaceState({}, "", "/auth/reset?token=reset-token");
    mount();
    await screen.findByRole("heading", { name: "Reset your password" }, { timeout: 8000 });
    type("New password", "new-password-1");
    type("Confirm new password", "new-password-1");
    screen.getByRole("button", { name: "Reset password & sign in" }).click();
    await expectSignInWithTheServersSentence("/api/auth/password/reset/confirm");
  }, 30000);

  it("sends the forced rotation's user to sign in, with the server's sentence", async () => {
    signedIn = false;
    window.history.replaceState({}, "", "/auth/password/change?email=r@example.test&temp=temporary");
    mount();
    await screen.findByRole("heading", { name: "Choose a new password" }, { timeout: 8000 });
    type("New password", "new-password-1");
    type("Confirm new password", "new-password-1");
    screen.getByRole("button", { name: "Set new password & sign in" }).click();
    await expectSignInWithTheServersSentence("/api/auth/password/change");
  }, 30000);

  // The other 409: nothing changed, so nothing is signed out and the form
  // stays where the user can try again.
  it("is not confused with a change already in progress, which keeps the form", async () => {
    answer = BUSY_409;
    await openAccountPasswordForm();
    type("Current password", "old-password");
    type("New password", "new-password-1");
    type("Confirm new password", "new-password-1");
    screen.getByRole("button", { name: "Change password" }).click();
    const shown = await screen.findByText(BUSY, {}, { timeout: 8000 });
    console.log("[409/busy] shown in the form:", JSON.stringify(shown.textContent));
    expect(screen.queryByRole("heading", { name: "Sign in to iterion" })).toBeNull();
    expect(screen.getByLabelText("Current password")).toBeTruthy();
    expect(posts.filter((p) => p === "/api/me/password")).toHaveLength(1);
    expect(posts.filter((p) => p === "/api/auth/logout")).toHaveLength(0);
  }, 30000);
});
