// @vitest-environment jsdom
//
// "Model providers" lists subscriptions only. A provider API key is the other
// model credential and lives on the team page, tab "API keys": the tab says so
// and links there. The second test follows the link into the REAL TeamPage, so
// a tab id that drifts on either side lands on the wrong tab and fails here.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Route, Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";

// The two panels the tab renders fetch on mount; neither is under test.
vi.mock("@/views/account/OAuthConnections", () => ({ default: () => null }));
vi.mock("@/views/account/CredentialPreview", () => ({ default: () => null }));

// TeamPage's surroundings. The team is whichever id the route carries, so the
// page shows the team the link named and no other.
vi.mock("@/hooks/useTenantSubject", () => ({
  useTeamSubject: (teamID: string) => ({
    subject: {
      teamID,
      name: `Team ${teamID}`,
      slug: teamID,
      role: "admin",
      personal: false,
      orgID: null,
      orgName: null,
      isMember: true,
    },
    loading: false,
    denied: false,
    notFound: false,
    error: null,
  }),
}));
vi.mock("@/hooks/useCanManageTeam", () => ({ useCanManageTeam: () => true }));
vi.mock("@/components/shared/useHeaderSlot", () => ({ useHeaderSlot: () => {} }));
vi.mock("@/auth/AuthContext", () => ({
  useAuth: () => ({ user: { id: "me", is_super_admin: false }, orgs: [] }),
}));
// The page's default tab (team access) reads these; they answer empty so that
// landing there by mistake fails on this file's assertion and not on a fetch.
vi.mock("@/api/byok", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/byok")>();
  return { ...actual, listTeamMembers: async () => [], listInvitations: async () => [] };
});
// The API keys tab's panel, reduced to a marker naming the team it was given.
vi.mock("@/views/account/ApiKeys", () => ({
  default: ({ team }: { team?: { id: string } }) => <div>API keys panel of {team?.id}</div>,
}));

import TeamPage from "@/views/teams/TeamPage";
import ModelProvidersTab from "./ModelProvidersTab";

afterEach(cleanup);

function mount() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  // The studio's own router base: the link is written against it.
  const loc = memoryLocation({ path: "/studio/integrations?tab=providers", record: true });
  render(
    <QueryClientProvider client={qc}>
      <Router hook={loc.hook} searchHook={loc.searchHook} base="/studio">
        <Route path="/integrations">
          <ModelProvidersTab teamID="team-a" />
        </Route>
        <Route path="/teams/:id">
          <TeamPage />
        </Route>
      </Router>
    </QueryClientProvider>,
  );
  return loc;
}

describe("ModelProvidersTab — where the API keys are", () => {
  it("says API keys are set on the team page and links to that team's tab", () => {
    mount();
    const link = screen.getByRole("link", { name: "API keys" });
    expect(link.getAttribute("href")).toBe("/studio/teams/team-a?tab=api-keys");
    // The sentence around the link names what is found there.
    expect(link.closest("p")?.textContent).toMatch(/API keys .* are set in the team settings/);
  });

  it("the link opens the API keys tab of that team", async () => {
    const loc = mount();
    fireEvent.click(screen.getByRole("link", { name: "API keys" }));
    expect(loc.history).toContain("/studio/teams/team-a?tab=api-keys");
    expect(await screen.findByText("API keys panel of team-a")).toBeTruthy();
    expect(screen.getByRole("tab", { name: "API keys" }).getAttribute("aria-selected")).toBe("true");
  });
});
