// @vitest-environment jsdom
// The connected-identities list distinguishes what "Sign out everywhere" does
// to each row: a link a signed-in session wrote is removed by it (the server
// marks those session_linked), the account's original sign-in link is kept.
// An older server sends no flag and the rows carry no promise.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

vi.mock("@/api/auth", async () => {
  const actual = await vi.importActual<typeof import("@/api/auth")>("@/api/auth");
  return {
    ...actual,
    listSSOLinks: vi.fn(async () => ({
      links: [
        { provider: "github", provider_user_id: "gh-1", email: "gh@example.test", created_at: "2026-01-01T00:00:00Z", session_linked: true },
        { provider: "oidc", provider_user_id: "kc-1", email: "kc@example.test", created_at: "2025-06-01T00:00:00Z", session_linked: false },
      ],
    })),
    listProviders: vi.fn(async () => ({ signup_mode: "invite_only", providers: [] })),
  };
});

vi.mock("@/auth/AuthContext", () => ({
  useAuth: () => ({
    user: { id: "u", email: "r@example.test", status: "active", is_super_admin: false },
    completeSignIn: vi.fn(async () => undefined),
    credentialSignedOut: vi.fn(),
    signOut: vi.fn(async () => undefined),
  }),
}));

vi.mock("@/views/account/ApiKeys", () => ({ default: () => null }));
vi.mock("@/views/account/OAuthConnections", () => ({ default: () => null }));
vi.mock("@/views/account/SharedQuota", () => ({ default: () => null }));
vi.mock("@/views/account/TokensPanel", () => ({ default: () => null }));
vi.mock("@/views/account/NotificationsPanel", () => ({ default: () => null }));

import SettingsPage from "@/views/account/SettingsPage";
import { useServerInfoStore } from "@/store/serverInfo";
import type { ServerInfo } from "@/api/types";

beforeEach(() => {
  vi.clearAllMocks();
  useServerInfoStore.setState({ info: { mode: "cloud", auth_required: true } as ServerInfo });
});

afterEach(() => {
  cleanup();
});

async function renderProfileTab(): Promise<void> {
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <SettingsPage />
    </QueryClientProvider>,
  );
  // Radix selects a tab on mousedown (the click follows).
  fireEvent.mouseDown(screen.getByRole("tab", { name: "Profile" }));
  fireEvent.click(screen.getByRole("tab", { name: "Profile" }));
  await screen.findByText("SSO connections", {}, { timeout: 8000 });
  await screen.findByText("github", {}, { timeout: 8000 });
}

describe("the connected SSO identities list", () => {
  it("marks the link a session wrote as the one sign out everywhere removes, and the original as kept", async () => {
    await renderProfileTab();
    const rows = screen.getAllByRole("listitem");
    expect(rows).toHaveLength(2);
    const github = rows.find((r) => within(r).queryByText("github") !== null)!;
    const oidc = rows.find((r) => within(r).queryByText("oidc") !== null)!;
    expect(within(github).getByText("Removed when you sign out everywhere")).toBeTruthy();
    expect(within(oidc).getByText("Kept when you sign out everywhere")).toBeTruthy();
  });

  it("promises nothing about rows an older server sent no flag for", async () => {
    const { listSSOLinks } = await import("@/api/auth");
    vi.mocked(listSSOLinks).mockResolvedValueOnce({
      links: [{ provider: "github", provider_user_id: "gh-1" }],
    });
    await renderProfileTab();
    const rows = screen.getAllByRole("listitem");
    expect(rows).toHaveLength(1);
    expect(within(rows[0]!).queryByText(/sign out everywhere/)).toBeNull();
    expect(within(rows[0]!).getByRole("button", { name: "Disconnect" })).toBeTruthy();
  });
});
