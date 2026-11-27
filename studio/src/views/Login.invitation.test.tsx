// @vitest-environment jsdom
import { Suspense, type ReactNode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

// The real App, AuthProvider, sign-in card and invitation view; only the
// destination views and the HTTP boundary are replaced. The server below
// answers as an invite-only one: a registration carrying an invitation
// claims it and signs the session for the team it joined.
vi.mock("@/components/shared/AppShell", () => ({ default: ({ children }: { children: ReactNode }) => <Suspense fallback={null}>{children}</Suspense> }));
vi.mock("@/components/shared/GlobalCommandPalette", () => ({ default: () => null }));
vi.mock("@/components/shared/Toast", () => ({ default: () => null }));
vi.mock("@/views/SettingsDialog", () => ({ default: () => null }));
vi.mock("@/components/shared/CloudReloginModal", () => ({ default: () => null }));
vi.mock("@/components/Home/HomeView", () => ({ default: () => <h1>Signed-in home</h1> }));
vi.mock("@/views/CloudLanding", () => ({ default: () => <h1>Public cloud home</h1>, PublicTopBar: () => null }));
vi.mock("@/views/RestrictedShell", () => ({ default: () => <h1>Restricted account</h1> }));
vi.mock("@/hooks/useProjectSwitchListener", () => ({ useProjectSwitchListener: () => undefined }));
vi.mock("@/hooks/useProjectScopeSync", () => ({ useProjectScopeSync: () => undefined }));
vi.mock("@/hooks/useDesktop", () => ({ useDesktop: () => ({ ready: true, isDesktop: false }) }));
vi.mock("@/views/teams/TeamPage", () => ({ default: () => <h1>Team page</h1> }));

import App from "@/App";
import { useServerInfoStore } from "@/store/serverInfo";
import type { ServerInfo } from "@/api/types";

const user = { id: "u", email: "invitee@example.test", status: "active", is_super_admin: false };
const identity = {
  user,
  orgs: [{ org_id: "o9", org_name: "Acme Org", org_slug: "acme", org_role: "admin", teams: [{ team_id: "t9", team_name: "Nine", team_slug: "nine", role: "member" }] }],
  active_org_id: "o9", active_team_id: "t9", active_role: "member",
};
const invitation = {
  email: "invitee@example.test",
  role: "member",
  org_role: "admin",
  team_id: "t9",
  team_name: "Nine",
  org_name: "Acme Org",
  invited_by: "owner@example.test",
};

let calls: string[];
let registered: unknown[];
let lookup: () => Response | Promise<Response>;
let signedIn: boolean;
let signupMode: "open" | "invite_only";
// registerTeam is the active team the registration's session answers: the
// invited team when the server honoured the invitation, a personal one when
// an open sign-up registered without reading it.
let registerTeam: string;

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

beforeEach(() => {
  calls = [];
  registered = [];
  signedIn = false;
  signupMode = "invite_only";
  registerTeam = "t9";
  lookup = () => json(invitation);
  useServerInfoStore.setState({ info: { mode: "cloud" } as ServerInfo });
  vi.stubGlobal("fetch", vi.fn(async (input: string | URL | Request, init?: RequestInit) => {
    const path = String(input).split("?")[0] ?? "";
    calls.push(`${init?.method ?? "GET"} ${path}`);
    if (path === "/api/server/info") return json({ auth_required: true, mode: "cloud" });
    if (path === "/api/auth/me") return signedIn ? json(identity) : json({ error: "unauthorized" }, 401);
    if (path === "/api/auth/refresh") return json({ error: "no refresh token" }, 401);
    if (path === "/api/auth/providers") return json({ signup_mode: signupMode, providers: [] });
    if (path === "/api/auth/invitations/lookup") return lookup();
    if (path === "/api/auth/register") {
      registered.push(JSON.parse(String(init?.body)));
      signedIn = true;
      return json({ user, active_org_id: "o9", active_team_id: registerTeam, active_role: "member" });
    }
    if (path === "/api/auth/login") {
      signedIn = true;
      return json({ user, active_org_id: "o9", active_team_id: "t1", active_role: "member" });
    }
    // The invitation this account accepted earlier: the accept names its team.
    if (path === "/api/auth/invitations/accept") {
      return json({ error: "identity: invitation already accepted", error_code: "invitation_accepted_by_you", team_id: "t9" }, 409);
    }
    if (path === "/api/auth/me/team/t9") return json({ user, active_org_id: "o9", active_team_id: "t9", active_role: "member" });
    return json({}, 200);
  }));
});

afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });

function openLogin(search: string) {
  window.history.replaceState({}, "", `/login${search}`);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={qc}><App /></QueryClientProvider>);
}

const LINK = `?invite=inv-1&next=${encodeURIComponent("/invitations/accept?token=inv-1")}`;

describe("registering through an invitation link", () => {
  it("names the invitation, locks the invited address and hides the token", async () => {
    openLogin(LINK);
    const summary = await screen.findByLabelText("Invitation", {}, { timeout: 6000 });
    for (const text of ["Nine", "Acme Org", "member", "admin", "owner@example.test"]) {
      expect(summary.textContent).toContain(text);
    }
    const email = screen.getByPlaceholderText("Email") as HTMLInputElement;
    await waitFor(() => expect(email.value).toBe("invitee@example.test"));
    expect(email.readOnly).toBe(true);
    // The providers answer (invite-only) is what would show a token field:
    // the SSO line saying none is configured for the address proves it landed.
    await screen.findByText(/No SSO is configured for that email/, {}, { timeout: 6000 });
    expect(screen.queryByPlaceholderText("Invitation token")).toBeNull();
    expect(screen.queryByDisplayValue("inv-1")).toBeNull();
    expect(screen.getByRole("button", { name: "Create account" })).toBeTruthy();
  }, 15000);

  it("lands the new account on the team it joined, never on the accept page", async () => {
    openLogin(LINK);
    const email = (await screen.findByPlaceholderText("Email", {}, { timeout: 6000 })) as HTMLInputElement;
    await waitFor(() => expect(email.value).toBe("invitee@example.test"));
    fireEvent.change(screen.getByPlaceholderText("Password"), { target: { value: "correct-horse" } });
    // Every address the browser is sent to from here, however briefly.
    const visited: string[] = [];
    for (const method of ["pushState", "replaceState"] as const) {
      const original = window.history[method].bind(window.history);
      vi.spyOn(window.history, method).mockImplementation((data, unused, url) => {
        if (url != null) visited.push(String(url));
        original(data, unused, url);
      });
    }
    fireEvent.click(screen.getByRole("button", { name: "Create account" }));
    await screen.findByRole("heading", { name: "Team page" }, { timeout: 8000 });
    expect(window.location.pathname).toBe("/studio/teams/t9");
    expect(visited.filter((u) => u.startsWith("/invitations/accept"))).toEqual([]);
    expect(registered).toEqual([{ email: "invitee@example.test", password: "correct-horse", name: "", invitation: "inv-1" }]);
    // The identity — the team list — is re-read after the registration.
    expect(calls.slice(calls.indexOf("POST /api/auth/register"))).toContain("GET /api/auth/me");
    expect(calls).not.toContain("POST /api/auth/invitations/accept");
    expect(calls.filter((c) => c === "GET /api/auth/invitations/lookup")).toHaveLength(1);
    expect(screen.queryByText(/already accepted/i)).toBeNull();
  }, 15000);

  it("shows a refused invitation instead of opening the form", async () => {
    lookup = () => json({ error: "invitation expired" }, 410);
    openLogin(LINK);
    const alert = await screen.findByText(/This invitation link can't be used: invitation expired/, {}, { timeout: 6000 });
    expect(alert).toBeTruthy();
    expect(screen.queryByPlaceholderText("Email")).toBeNull();
    expect(screen.queryByRole("button", { name: "Create account" })).toBeNull();
    // The way out reveals the manual form, the link's token in it.
    fireEvent.click(screen.getByRole("button", { name: "Use another invitation or address" }));
    expect((screen.getByPlaceholderText("Email") as HTMLInputElement).readOnly).toBe(false);
    expect((await screen.findByPlaceholderText("Invitation token") as HTMLInputElement).value).toBe("inv-1");
  }, 15000);

  it("frees the address and shows the token once the visitor leaves the link's form", async () => {
    openLogin(LINK);
    const email = (await screen.findByPlaceholderText("Email", {}, { timeout: 6000 })) as HTMLInputElement;
    await waitFor(() => expect(email.value).toBe("invitee@example.test"));
    fireEvent.click(screen.getByRole("button", { name: "Use another invitation or address" }));
    const free = screen.getByPlaceholderText("Email") as HTMLInputElement;
    expect(free.readOnly).toBe(false);
    fireEvent.change(free, { target: { value: "other@example.test" } });
    expect(free.value).toBe("other@example.test");
    const token = (await screen.findByPlaceholderText("Invitation token")) as HTMLInputElement;
    expect(token.value).toBe("inv-1");
    expect(screen.queryByLabelText("Invitation")).toBeNull();
  }, 15000);
});

describe("an invitation link whose lookup does not settle the form", () => {
  it("keeps the way out while the lookup has not answered", async () => {
    lookup = () => new Promise<Response>(() => {});
    openLogin(LINK);
    await screen.findByText("Checking your invitation…", {}, { timeout: 6000 });
    fireEvent.click(screen.getByRole("button", { name: "Use another invitation or address" }));
    expect((screen.getByPlaceholderText("Email") as HTMLInputElement).readOnly).toBe(false);
  }, 15000);

  it("sends the acceptee of an already-accepted invitation to sign in, then to its team", async () => {
    lookup = () => json({ error: "invitation already accepted" }, 409);
    openLogin(LINK);
    await screen.findByText(/already been accepted\. If you accepted it, sign in to open its team/, {}, { timeout: 6000 });
    const email = screen.getByPlaceholderText("Email") as HTMLInputElement;
    expect(email.readOnly).toBe(false);
    fireEvent.change(email, { target: { value: "invitee@example.test" } });
    fireEvent.change(screen.getByPlaceholderText("Password"), { target: { value: "correct-horse" } });
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
    await screen.findByRole("heading", { name: "Team page" }, { timeout: 8000 });
    expect(window.location.pathname).toBe("/studio/teams/t9");
    expect(calls).toContain("POST /api/auth/login");
    expect(calls).not.toContain("POST /api/auth/register");
  }, 15000);
});

describe("an open sign-up through an invitation link", () => {
  it("sends the account to the accept page, the invitation not consumed by the registration", async () => {
    signupMode = "open";
    registerTeam = "personal";
    openLogin(LINK);
    const email = (await screen.findByPlaceholderText("Email", {}, { timeout: 6000 })) as HTMLInputElement;
    await waitFor(() => expect(email.value).toBe("invitee@example.test"));
    fireEvent.change(screen.getByPlaceholderText("Password"), { target: { value: "correct-horse" } });
    fireEvent.click(screen.getByRole("button", { name: "Create account" }));
    await waitFor(() => expect(window.location.pathname).toBe("/invitations/accept"), { timeout: 8000 });
    expect(new URLSearchParams(window.location.search).get("token")).toBe("inv-1");
  }, 15000);
});

describe("an open sign-up through an invitation link whose lookup failed", () => {
  it("sends the account to the accept page, not to the personal team", async () => {
    signupMode = "open";
    registerTeam = "personal";
    lookup = () => json({ error: "lookup unavailable" }, 503);
    openLogin(LINK);
    fireEvent.click(await screen.findByRole("button", { name: "Use another invitation or address" }, { timeout: 6000 }));
    fireEvent.change(screen.getByPlaceholderText("Email"), { target: { value: "invitee@example.test" } });
    fireEvent.change(screen.getByPlaceholderText("Password"), { target: { value: "correct-horse" } });
    // The open mode is known before the submit: the SSO line answers once the
    // providers landed.
    await screen.findByText(/No SSO is configured for that email/, {}, { timeout: 6000 });
    fireEvent.click(screen.getByRole("button", { name: "Create account" }));
    await waitFor(() => expect(window.location.pathname).toBe("/invitations/accept"), { timeout: 8000 });
    expect(new URLSearchParams(window.location.search).get("token")).toBe("inv-1");
    expect(registered).toEqual([{ email: "invitee@example.test", password: "correct-horse", name: "", invitation: "inv-1" }]);
  }, 15000);
});

describe("leaving the link's form for another invitation", () => {
  it("lands a registration with another token on the team its session joined", async () => {
    openLogin(LINK);
    const email = (await screen.findByPlaceholderText("Email", {}, { timeout: 6000 })) as HTMLInputElement;
    await waitFor(() => expect(email.value).toBe("invitee@example.test"));
    fireEvent.click(screen.getByRole("button", { name: "Use another invitation or address" }));
    fireEvent.change(screen.getByPlaceholderText("Email"), { target: { value: "other@example.test" } });
    fireEvent.change(screen.getByPlaceholderText("Password"), { target: { value: "correct-horse" } });
    fireEvent.change(await screen.findByPlaceholderText("Invitation token"), { target: { value: "typed-token" } });
    fireEvent.click(screen.getByRole("button", { name: "Create account" }));
    await screen.findByRole("heading", { name: "Team page" }, { timeout: 8000 });
    expect(window.location.pathname).toBe("/studio/teams/t9");
    expect(registered).toEqual([{ email: "other@example.test", password: "correct-horse", name: "", invitation: "typed-token" }]);
    expect(calls).not.toContain("POST /api/auth/invitations/accept");
  }, 15000);
});

describe("registering without a link", () => {
  it("keeps today's form: free address, no invitation, no lookup", async () => {
    signupMode = "open";
    openLogin("");
    fireEvent.click(await screen.findByRole("button", { name: "Need an account? Sign up" }, { timeout: 6000 }));
    const email = screen.getByPlaceholderText("Email") as HTMLInputElement;
    expect(email.readOnly).toBe(false);
    fireEvent.change(email, { target: { value: "new@example.test" } });
    fireEvent.change(screen.getByPlaceholderText("Password"), { target: { value: "correct-horse" } });
    expect(screen.queryByPlaceholderText("Invitation token")).toBeNull();
    expect(screen.queryByLabelText("Invitation")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Create account" }));
    await waitFor(() => expect(registered).toEqual([{ email: "new@example.test", password: "correct-horse", name: "" }]));
    await screen.findByRole("heading", { name: "Signed-in home" }, { timeout: 8000 });
    expect(calls).not.toContain("GET /api/auth/invitations/lookup");
  }, 15000);
});
