// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import type { Delivery, ShareView } from "@/api/configShareAdmin";

import { ShareDeliveriesDrawer } from "./ShareDeliveriesDrawer";

// Mock only the network call; keep every other real export so transitive
// imports still resolve.
const listConfigShareDeliveries = vi.fn<() => Promise<Delivery[]>>();
vi.mock("@/api/configShareAdmin", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/configShareAdmin")>();
  return {
    ...actual,
    listConfigShareDeliveries: (...args: unknown[]) =>
      listConfigShareDeliveries(...(args as [])),
  };
});

const share: ShareView = {
  id: "sh-1",
  bot_id: "feed-watch",
  label: "Veille A11y — Alice",
  repo_url: "https://github.com/org/repo",
  repo_ref: "main",
  config_path: "config.yaml",
  category: "a11y",
  schema_ref: "",
  allowed_paths: ["feeds"],
  visible_paths: ["feeds"],
  read_only: false,
  enabled: true,
  token_last4: "abcd",
  fingerprint: "fp",
  created_at: new Date().toISOString(),
  expires_at: "",
};

afterEach(() => {
  cleanup();
  listConfigShareDeliveries.mockReset();
});

function renderDrawer() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <ShareDeliveriesDrawer teamID="t1" share={share} onClose={() => {}} />
    </QueryClientProvider>,
  );
}

describe("ShareDeliveriesDrawer", () => {
  it("renders the empty state when the share was never fetched", async () => {
    listConfigShareDeliveries.mockResolvedValue([]);
    renderDrawer();
    // Matched on the leading sentence, not the whole paragraph: the rest of
    // the copy explains what the log excludes and is reworded often.
    expect(
      await screen.findByText(/No edits through this share yet\./),
    ).toBeTruthy();
  });

  it("lists deliveries with status, actor and error", async () => {
    listConfigShareDeliveries.mockResolvedValue([
      {
        id: "d1",
        share_id: "sh-1",
        tenant_id: "t1",
        at: new Date().toISOString(),
        source_ip: "10.0.0.1",
        user_agent: "curl",
        method: "PUT",
        actor: "share:sh-1",
        status: 200,
        changed_paths: ["a11y.feeds"],
      },
      {
        id: "d2",
        share_id: "sh-1",
        tenant_id: "t1",
        at: new Date().toISOString(),
        source_ip: "10.0.0.2",
        user_agent: "curl",
        method: "GET",
        status: 403,
        error: "share revoked",
      },
    ]);
    renderDrawer();
    expect(await screen.findByText("share:sh-1")).toBeTruthy();
    expect(screen.getByText("200")).toBeTruthy();
    expect(screen.getByText("403")).toBeTruthy();
    expect(screen.getByText("a11y.feeds")).toBeTruthy();
    expect(screen.getByText("share revoked")).toBeTruthy();
    // Legacy row without an actor falls back to the source IP.
    expect(screen.getByText("10.0.0.2")).toBeTruthy();
  });

  it("surfaces a load failure", async () => {
    listConfigShareDeliveries.mockRejectedValue(new Error("boom"));
    renderDrawer();
    expect(await screen.findByText(/boom/)).toBeTruthy();
  });
});
