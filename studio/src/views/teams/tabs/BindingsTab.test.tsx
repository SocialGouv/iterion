// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { BotSecretBinding } from "@/api/botBindings";
import type { BotEntryWithSchema } from "@/api/bots";
import { useBotsStore } from "@/store/bots";

import BindingsTab from "./BindingsTab";

// The context binding the table must render as ITSELF — a pinned source and
// the engine's readout — not as a secret binding whose secret is missing.
const contextBinding: BotSecretBinding = {
  id: "b1",
  tenant_id: "t1",
  bot_id: "doki",
  kind: "context",
  secret_id: "",
  secret_name_for_workflow: "handbook",
  context: {
    git_url: "https://gitlab.example.com/org/doki-context.git",
    ref: "v1.4.2",
    path: "handbook/pricing.pdf",
  },
  degraded_reason: "fetch the pinned source: the remote refused the pin",
  degraded_at: "2026-11-03T08:00:00Z",
  created_by: "alice",
  created_at: "2026-11-01T10:00:00Z",
  updated_at: "2026-11-01T10:00:00Z",
};

vi.mock("@/api/botBindings", async (importActual) => {
  const actual = await importActual<typeof import("@/api/botBindings")>();
  return {
    ...actual,
    listBindings: vi.fn(async () => [contextBinding]),
    createBinding: vi.fn(),
    updateBinding: vi.fn(),
    deleteBinding: vi.fn(),
  };
});
vi.mock("@/api/secrets", () => ({
  listTeamSecrets: vi.fn(async () => []),
}));

afterEach(cleanup);

function renderTab() {
  useBotsStore.setState({
    bots: [{ name: "doki" } as unknown as BotEntryWithSchema],
    loading: false,
    error: null,
  });
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <BindingsTab teamID="t1" canManage />
    </QueryClientProvider>,
  );
}

describe("BindingsTab on a context binding", () => {
  it("shows the kind, the pinned source, and the degraded readout", async () => {
    renderTab();
    // The workflow name is the row's anchor.
    await screen.findByText("handbook");
    // The kind badge — a context binding must not read as a secret one.
    expect(screen.getByText("context")).toBeTruthy();
    // The source cell names the pin, not a missing secret. The pin's text
    // is fragmented across JSX nodes, so it is matched on the cell's
    // textContent.
    const sourceCell = screen.getByText(/doki-context\.git/);
    expect(sourceCell.textContent).toContain("@");
    expect(sourceCell.textContent).toContain("v1.4.2");
    expect(screen.getByText("handbook/pricing.pdf")).toBeTruthy();
    expect(screen.queryByText("Bound secret was deleted")).toBeNull();
    // The engine's readout is visible: the badge and its reason.
    expect(screen.getByText("degraded")).toBeTruthy();
    expect(screen.getByText(/remote refused the pin/)).toBeTruthy();
  });

  it("locks the kind in the edit dialog", async () => {
    renderTab();
    await screen.findByText("handbook");
    fireEvent.click(screen.getByRole("button", { name: "Edit" }));
    await screen.findByText("Edit binding");
    // The kind is immutable server-side (a PATCH never applies it), so the
    // dialog shows it — it must not offer the create-time kind picker.
    expect(
      screen.queryByText("secret — a team credential exposed to the workflow"),
    ).toBeNull();
    // And the context fields stay editable, pre-filled from the record.
    expect(screen.getByDisplayValue("v1.4.2")).toBeTruthy();
    expect(screen.getByDisplayValue("handbook/pricing.pdf")).toBeTruthy();
  });
});
