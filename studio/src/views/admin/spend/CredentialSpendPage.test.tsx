// @vitest-environment jsdom
//
// The loading / error / empty triad of the credential-spend table. The pure
// helpers are covered in credentialSpend.test.ts; what needs a render is the
// branch that CHOOSES between them, because the defect it shipped with was a
// failed fetch reading as an authoritative "no spend recorded" — a false
// zero-spend claim no helper test can see. (The full a11y + gating harness for
// this screen is T9/#1449; this is the regression guard for that one branch.)
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

// The page is gated on a super-admin identity and a cloud server; stub the
// identity so what is under test is the table, not the gate.
vi.mock("@/auth/AuthContext", () => ({
  useAuth: () => ({ user: { is_super_admin: true } }),
}));

// vi.hoisted, not a plain const: the factory below reads the stub eagerly
// (through the spread), and vi.mock is hoisted above this file's imports.
const { getAdminCredentialUsage } = vi.hoisted(() => ({
  getAdminCredentialUsage: vi.fn(),
}));

// Keep the module's real FeatureUnavailableError — the page branches on it
// with `instanceof`, so a stubbed class would silently never match.
vi.mock("@/api/adminCredUsage", async () => {
  const actual =
    await vi.importActual<typeof import("@/api/adminCredUsage")>("@/api/adminCredUsage");
  return { ...actual, getAdminCredentialUsage };
});

import CredentialSpendPage from "./CredentialSpendPage";
import type { CredentialUsageListView, CredentialUsageView } from "@/api/adminCredUsage";
import type { ServerInfo } from "@/api/types";
import { useServerInfoStore } from "@/store/serverInfo";

const answer = (credentials: CredentialUsageView[]): CredentialUsageListView => ({
  month: "2026-09",
  metered_usd: 0,
  estimated_usd: 0,
  scope: { tier: "platform" },
  credentials,
});

function renderPage() {
  // retry off: what is asserted is the state a failed fetch LANDS in, not the
  // client's backoff on the way there.
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <CredentialSpendPage />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
  useServerInfoStore.setState({ info: { mode: "cloud" } as ServerInfo });
});

afterEach(() => cleanup());

describe("CredentialSpendPage", () => {
  it("does not report a failed request as a zero-spend answer", async () => {
    getAdminCredentialUsage.mockRejectedValue(new Error("upstream exploded"));

    renderPage();

    expect(await screen.findByText(/not a zero-spend answer/)).toBeTruthy();
    expect(screen.queryByText(/No spend recorded/)).toBeNull();
  });

  it("keeps the zero-spend wording for a server answer that is genuinely empty", async () => {
    getAdminCredentialUsage.mockResolvedValue(answer([]));

    renderPage();

    expect(await screen.findByText(/No spend recorded/)).toBeTruthy();
  });

  // One row can merge a split-reporting backend and a CLI delegate, so it
  // carries a split AND an aggregate; `backends` is what lets the reader tell
  // why the figure is mixed.
  it("shows a mixed row's aggregate tokens and the backends that produced them", async () => {
    getAdminCredentialUsage.mockResolvedValue(
      answer([
        {
          fingerprint: "sha256:abcd",
          provider: "anthropic",
          tier: "pool",
          nature: "metered",
          month: "2026-09",
          cost_usd: 13.68,
          input_tokens: 1000,
          output_tokens: 250,
          aggregate_tokens: 5000,
          runs: 7,
          backends: ["claude_code", "claw"],
        },
      ]),
    );

    renderPage();

    expect(await screen.findByText(/\(aggregate\)/)).toBeTruthy();
    expect(screen.getByText(/1,000 in \/ 250 out/)).toBeTruthy();
    expect(screen.getByText("claude_code, claw")).toBeTruthy();
  });
});
