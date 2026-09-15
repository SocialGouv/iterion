// @vitest-environment jsdom
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import type { OAuthConnection } from "@/api/byok";
import OAuthConnections from "./OAuthConnections";

const api = vi.hoisted(() => ({
  listOAuthConnections: vi.fn(), refreshOAuth: vi.fn(), deleteOAuth: vi.fn(),
  renameOAuth: vi.fn(), startOAuthAuthorize: vi.fn(), completeOAuthAuthorize: vi.fn(),
  uploadOAuthCredentials: vi.fn(),
}));
vi.mock("@/api/byok", () => api);
afterEach(() => { cleanup(); vi.clearAllMocks(); });

function connections(): OAuthConnection[] {
  return [0, 2].map((rank) => ({
    kind: "claude_code", rank, account_label: `Slot ${rank}`,
    account_email: "account@example.org", account_verified: true,
    account_checked_at: "2026-09-14T08:00:00Z", refreshable: true,
    fingerprint: "account:anthropic:123456789abcdef",
    created_at: "2026-09-14T08:00:00Z", updated_at: "2026-09-14T08:00:00Z",
  }));
}
function setup() {
  api.listOAuthConnections.mockResolvedValue(connections());
  api.refreshOAuth.mockResolvedValue(connections()[1]);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const tree = (teamId: string) => <QueryClientProvider client={client}><OAuthConnections scope={{ teamId }} org /></QueryClientProvider>;
  return { ...render(tree("alpha")), tree };
}

it("refreshes the selected fallback and shows its shared provider quota", async () => {
  setup();
  const select = await screen.findByRole("combobox", { name: "Claude Code chain entry" });
  fireEvent.change(select, { target: { value: "2" } });
  expect(screen.getByText("Slot 2")).toBeTruthy();
  expect(screen.getByText(/These entries share one provider quota/)).toBeTruthy();
  expect(screen.getByText("123456789ab")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Refresh tokens" }));
  await waitFor(() => expect(api.refreshOAuth).toHaveBeenCalledWith("claude_code", { teamId: "alpha" }, 2));
});

it("keeps a connect form on its entry and discards it on owner changes", async () => {
  const view = setup();
  const select = await screen.findByRole("combobox", { name: "Claude Code chain entry" });
  fireEvent.change(select, { target: { value: "3" } });
  fireEvent.click(screen.getByRole("button", { name: "Advanced: paste file" }));
  expect((select as HTMLSelectElement).disabled).toBe(true);
  expect(screen.getByRole("button", { name: "Save" })).toBeTruthy();
  view.rerender(view.tree("beta"));
  await screen.findByRole("combobox", { name: "Claude Code chain entry" });
  expect(screen.queryByRole("button", { name: "Save" })).toBeNull();
  expect((screen.getByRole("combobox", { name: "Claude Code chain entry" }) as HTMLSelectElement).value).toBe("0");
  expect(api.uploadOAuthCredentials).not.toHaveBeenCalled();
});
