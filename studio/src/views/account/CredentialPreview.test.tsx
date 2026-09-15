// @vitest-environment jsdom
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import type { CredentialPreview as Preview } from "@/api/credentialPreview";
import CredentialPreview from "./CredentialPreview";

const api = vi.hoisted(() => ({ previewCredentials: vi.fn(), listBots: vi.fn(), listWebhooks: vi.fn() }));
vi.mock("@/api/credentialPreview", () => ({ previewCredentials: api.previewCredentials }));
vi.mock("@/api/bots", () => ({ listBots: api.listBots }));
vi.mock("@/api/webhooks", () => ({ listWebhooks: api.listWebhooks }));
afterEach(() => { cleanup(); vi.clearAllMocks(); });

function observation(): Preview {
  return {
    observed_at: "2026-09-14T08:00:00Z",
    context: { team_id: "team-a", bot_id: "review-pr", source: { kind: "personal" } },
    wires: [{ wire: "anthropic", candidate_ids: ["a", "b"] }],
    candidates: [
      { id: "a", tier: "team", source: "oauth", provider: "anthropic", wire: "anthropic", rank: 0, label: "Primary account", state: "window_closed", reason: "Provider refused", pinned: false, selected: false, selection: "shadowed", conditional: true, account_group: "group-1", windows: [{ name: "seven_day", percent: 0, status: "rejected", fresh: true, observed_at: "2026-09-14T08:00:00Z" }] },
      { id: "b", tier: "platform", source: "oauth", provider: "anthropic", wire: "anthropic", rank: 1, label: "Platform subscription", state: "unknown", reason: "Needs a provider observation", pinned: false, selected: true, selection: "selected", conditional: true, account_group: "group-1", windows: [] },
    ],
    pool: { considered: false, reason: "tenant credentials present", wants: [] },
    warnings: ["Capacity is not reserved."],
  };
}

function setup() {
  api.listBots.mockResolvedValue([{ name: "review-pr", display_name: "Revi" }]);
  api.listWebhooks.mockResolvedValue([{ id: "hook-a", name: "Repository reviews", bot_ids: ["review-pr"], default_bot_id: "review-pr", enabled: true }]);
  api.previewCredentials.mockResolvedValue(observation());
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><CredentialPreview teamID="team-a" /></QueryClientProvider>);
}

it("keeps the server order, displays correlation, and does not turn a refusal into zero usage", async () => {
  setup();
  await screen.findByRole("option", { name: "Revi (review-pr)" });
  fireEvent.change(screen.getByRole("combobox", { name: "Credential preview bot" }), { target: { value: "review-pr" } });
  fireEvent.click(screen.getByRole("button", { name: "Preview fallback chain" }));
  await screen.findByText("Team · Primary account");
  expect(api.previewCredentials).toHaveBeenCalledWith("team-a", { source: { kind: "personal" }, bot_id: "review-pr" });
  const rows = screen.getAllByRole("listitem");
  expect(rows[0]?.textContent).toContain("Primary account");
  expect(rows[1]?.textContent).toContain("Platform subscription");
  expect(screen.getAllByText("Shared account quota")).toHaveLength(2);
  expect(screen.getByText(/provider refused; usage unknown/)).toBeTruthy();
  expect(screen.queryByText(/0% used/)).toBeNull();
});

it("previews the selected webhook without substituting the reader's personal identity", async () => {
  setup();
  fireEvent.change(screen.getByRole("combobox", { name: "Credential preview launch source" }), { target: { value: "webhook" } });
  await screen.findByRole("option", { name: "Repository reviews" });
  fireEvent.change(screen.getByRole("combobox", { name: "Credential preview webhook" }), { target: { value: "hook-a" } });
  fireEvent.click(screen.getByRole("button", { name: "Preview fallback chain" }));
  await waitFor(() => expect(api.previewCredentials).toHaveBeenCalledWith("team-a", { source: { kind: "webhook", id: "hook-a" } }));
});

it("hides a previous observation as soon as its source changes", async () => {
  setup();
  await screen.findByRole("option", { name: "Revi (review-pr)" });
  fireEvent.change(screen.getByRole("combobox", { name: "Credential preview bot" }), { target: { value: "review-pr" } });
  fireEvent.click(screen.getByRole("button", { name: "Preview fallback chain" }));
  await screen.findByText("Team · Primary account");
  fireEvent.change(screen.getByRole("combobox", { name: "Credential preview launch source" }), { target: { value: "webhook" } });
  expect(screen.queryByText("Team · Primary account")).toBeNull();
  expect(api.previewCredentials).toHaveBeenCalledTimes(1);
});
