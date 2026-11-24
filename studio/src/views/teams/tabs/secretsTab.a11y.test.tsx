// @vitest-environment jsdom
//
// The team secrets tab renders TWO tables — team and personal — each
// repeating "Rotate" and "Delete" per row, so one secret name present in
// both scopes leaves four controls announcing two names (#2591). axe has no
// rule for that; only this pin bites. The fixture holds exactly that case.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { GenericSecretView } from "@/api/secrets";
import {
  expectNoViolationsStrict,
  expectUniqueAccessibleNames,
  setupMatchMedia,
} from "@/__tests__/a11y/axeHelpers";

const listTeamSecrets = vi.fn<() => Promise<GenericSecretView[]>>();
const listMySecrets = vi.fn<() => Promise<GenericSecretView[]>>();
vi.mock("@/api/secrets", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/secrets")>();
  return {
    ...actual,
    listTeamSecrets: () => listTeamSecrets(),
    listMySecrets: () => listMySecrets(),
  };
});
vi.mock("wouter", () => ({ useLocation: () => ["/teams/t-1", vi.fn()] }));

import SecretsTab from "./SecretsTab";

setupMatchMedia();

const secret = (id: string, name: string): GenericSecretView => ({
  id,
  name,
  last4: id.slice(-4),
  fingerprint: `fp-${id}`,
  created_at: "2026-10-01T10:00:00Z",
  last_used_at: "2026-10-30T10:00:00Z",
});

// One name in BOTH scopes: the id is unique, the name is not, and the tab
// shows the two tables side by side.
const TEAM = [secret("t-0001", "ANTHROPIC_KEY"), secret("t-0002", "OVH_TOKEN")];
const MINE = [secret("m-0001", "ANTHROPIC_KEY")];

function mountTab(canManage = true): HTMLElement {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return render(
    <QueryClientProvider client={qc}>
      <main>
        <SecretsTab teamID="t-1" canManage={canManage} />
      </main>
    </QueryClientProvider>,
  ).container;
}

beforeEach(() => {
  listTeamSecrets.mockReset();
  listMySecrets.mockReset();
  listTeamSecrets.mockResolvedValue(TEAM);
  listMySecrets.mockResolvedValue(MINE);
});
afterEach(cleanup);

describe("team secrets naming (RGAA 11.1)", () => {
  it("names each row's action after the secret AND the table it belongs to", async () => {
    mountTab();
    await screen.findByRole("table", { name: "Team secrets" });
    for (const name of [
      "Rotate ANTHROPIC_KEY (team)",
      "Delete ANTHROPIC_KEY (team)",
      "Rotate OVH_TOKEN (team)",
      "Delete OVH_TOKEN (team)",
      "Rotate ANTHROPIC_KEY (personal)",
      "Delete ANTHROPIC_KEY (personal)",
    ]) {
      expect(screen.getAllByRole("button", { name })).toHaveLength(1);
    }
  });

  it("keeps the visible word first, so the name contains the label (SC 2.5.3)", async () => {
    mountTab();
    await screen.findByRole("table", { name: "Team secrets" });
    expect(
      screen.getByRole("button", { name: "Rotate ANTHROPIC_KEY (team)" }).textContent,
    ).toBe("Rotate");
  });

  it("leaves no two row controls sharing an accessible name across the two tables", async () => {
    const container = mountTab();
    await screen.findByRole("table", { name: "Team secrets" });
    expectUniqueAccessibleNames(container, "Team secrets · rows", {
      roles: [{ role: "button", matching: [/^Rotate /, /^Delete /] }],
      count: 6,
    });
  });

  it("renders no row action at all for a member who cannot manage the team's secrets", async () => {
    mountTab(false);
    await screen.findByRole("table", { name: "Team secrets" });
    expect(screen.queryByRole("button", { name: "Rotate ANTHROPIC_KEY (team)" })).toBeNull();
    // Personal secrets stay manageable whatever the team role is.
    expect(screen.getByRole("button", { name: "Rotate ANTHROPIC_KEY (personal)" })).toBeTruthy();
  });
});

describe("team secrets axe", () => {
  it("SecretsTab — both tables loaded, axe smoke, incompletes included", async () => {
    const container = mountTab();
    await screen.findByRole("table", { name: "Team secrets" });
    await screen.findByRole("table", { name: "My secrets" });
    await expectNoViolationsStrict(container, "SecretsTab");
  });
});
