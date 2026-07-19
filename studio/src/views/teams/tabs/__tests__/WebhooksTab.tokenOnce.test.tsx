// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

// Stub the API surface WebhooksTab uses so the page can render with a
// controllable set of webhooks. The Create flow calls createWebhook —
// we wire it to return a fixed token to exercise the token-once panel.
const sampleWebhook = {
  id: "wh_42",
  tenant_id: "team_1",
  name: "Demo",
  provider: "gitlab" as const,
  enabled: true,
  token_last4: "abcd",
  fingerprint: "fp",
  bot_ids: ["whats-next"],
  wildcard_bots: false,
  rate_limit: { rate: 1, burst: 10 },
  created_by: "u",
  created_at: new Date().toISOString(),
  updated_at: new Date().toISOString(),
};

vi.mock("@/api/webhooks", async () => {
  const actual = await vi.importActual<typeof import("@/api/webhooks")>("@/api/webhooks");
  return {
    ...actual,
    listWebhooks: vi.fn(async () => [sampleWebhook]),
    createWebhook: vi.fn(),
    rotateWebhook: vi.fn(async () => ({ config: sampleWebhook, token: "rotated_iwh_token_xyz" })),
    deleteWebhook: vi.fn(),
    updateWebhook: vi.fn(),
    listWebhookDeliveries: vi.fn(async () => []),
  };
});

vi.mock("@/api/bots", () => ({
  listBots: vi.fn(async () => [
    { name: "whats-next", display_name: "Nexie", path: "/x" },
  ]),
}));

import WebhooksTab from "@/views/teams/tabs/WebhooksTab";
import * as webhooksApi from "@/api/webhooks";

beforeEach(() => {
  vi.clearAllMocks();
});
afterEach(() => {
  cleanup();
});

describe("WebhooksTab token-once panel", () => {
  it("renders the issued token + inbound URL after a rotate", async () => {
    // The tab reads the webhook list through react-query — provide a
    // client (retry off so a mock failure surfaces immediately).
    render(
      <QueryClientProvider
        client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}
      >
        <WebhooksTab teamID="team_1" canManage />
      </QueryClientProvider>,
    );

    // Wait for the row to appear.
    await screen.findByText("Demo");

    // Click the row's Rotate button → opens the confirm dialog.
    fireEvent.click(screen.getByRole("button", { name: /^Rotate$/i }));
    // Confirm — click the primary Rotate button inside the open
    // ConfirmDialog (queried by role so the markup can evolve).
    const dialog = await screen.findByRole("dialog", { name: /Rotate Demo\?/ });
    fireEvent.click(within(dialog).getByRole("button", { name: /^Rotate$/i }));

    await waitFor(() => {
      expect(webhooksApi.rotateWebhook).toHaveBeenCalledWith("team_1", "wh_42");
    });

    // The token-once panel surfaces both the inbound URL and the new
    // plaintext token.
    const panel = await screen.findByTestId("token-once-panel");
    expect(panel.textContent).toContain("rotated_iwh_token_xyz");
    expect(panel.textContent).toMatch(/\/api\/webhooks\/gitlab\/wh_42/);
  });
});
