// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import type { ForgeOAuthApp } from "@/api/forgeConnections";

import { OAuthAppsSection } from "../integrations/OAuthAppsSection";

const teamApp = {
  id: "app-team",
  tenant_id: "t1",
  provider: "gitlab",
  forge_base_url: "https://gitlab.example",
  client_id: "team-cid",
  auto_created: false,
  created_at: "2026-10-01T00:00:00Z",
} as unknown as ForgeOAuthApp;

const platformApp = {
  id: "app-platform",
  tenant_id: "platform",
  provider: "gitlab",
  forge_base_url: "https://pic.example",
  client_id: "platform-cid",
  platform: true,
  created_at: "2026-10-01T00:00:00Z",
} as unknown as ForgeOAuthApp;

afterEach(cleanup);

// A platform app serves the team but is not the team's: the row says so and
// offers no action — the team's Delete would only answer 404.
describe("OAuthAppsSection — platform apps in the team list", () => {
  it("marks the platform app and offers actions only on the team's own", () => {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={qc}>
        <OAuthAppsSection
          teamID="t1"
          apps={[teamApp, platformApp]}
          connections={[]}
          canManage
          onChanged={vi.fn()}
          onError={vi.fn()}
          confirm={vi.fn()}
        />
      </QueryClientProvider>,
    );
    expect(screen.getByText("platform")).toBeTruthy();
    expect(screen.getByText("manual")).toBeTruthy();
    const deletes = screen.getAllByRole("button", { name: "Delete" });
    expect(deletes).toHaveLength(1);
    expect(screen.getByText(/platform-cid/)).toBeTruthy();
  });
});
