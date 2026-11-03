// @vitest-environment jsdom
//
// The local secrets table repeats "Rotate" and "Delete" on every row, and
// axe has NO rule for two controls answering to one name — only a colocated
// pin bites (#2591). The fixture holds the hostile case the surface really
// produces: ONE name at both scopes, which is what the table's own key
// (`${scope}:${id}`) says is distinct.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { LocalSecretView } from "@/api/secrets";
import {
  expectNoViolationsStrict,
  expectUniqueAccessibleNames,
  setupMatchMedia,
} from "@/__tests__/a11y/axeHelpers";

const listLocalSecrets = vi.fn<() => Promise<LocalSecretView[]>>();
vi.mock("@/api/secrets", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/api/secrets")>();
  return { ...actual, listLocalSecrets: () => listLocalSecrets() };
});

import Secrets from "./Secrets";

setupMatchMedia();

const SECRETS: LocalSecretView[] = [
  {
    id: "s-1",
    name: "ANTHROPIC_KEY",
    scope: "global",
    last4: "9f21",
    allowed_hosts: ["api.anthropic.com"],
    created_at: "2026-10-01T10:00:00Z",
  },
  // Same NAME, other scope: the row key is the (scope, id) pair, so the
  // accessible name has to carry both or these two rows are one control.
  {
    id: "s-2",
    name: "ANTHROPIC_KEY",
    scope: "project",
    last4: "4ab0",
    created_at: "2026-10-02T10:00:00Z",
  },
  { id: "s-3", name: "OVH_TOKEN", scope: "project", created_at: "2026-10-03T10:00:00Z" },
];

function mountSecrets(): HTMLElement {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  return render(
    <QueryClientProvider client={qc}>
      <main>
        <Secrets />
      </main>
    </QueryClientProvider>,
  ).container;
}

beforeEach(() => {
  listLocalSecrets.mockReset();
  listLocalSecrets.mockResolvedValue(SECRETS);
});
afterEach(cleanup);

describe("local secrets naming (RGAA 11.1)", () => {
  it("names every row's action after the secret AND the scope it acts on", async () => {
    mountSecrets();
    await screen.findByRole("table", { name: "Local secrets" });
    for (const name of [
      "Rotate ANTHROPIC_KEY (global)",
      "Delete ANTHROPIC_KEY (global)",
      "Rotate ANTHROPIC_KEY (project)",
      "Delete ANTHROPIC_KEY (project)",
      "Rotate OVH_TOKEN (project)",
      "Delete OVH_TOKEN (project)",
    ]) {
      expect(screen.getAllByRole("button", { name })).toHaveLength(1);
    }
  });

  it("keeps the visible word first, so the name contains the label (SC 2.5.3)", async () => {
    mountSecrets();
    await screen.findByRole("table", { name: "Local secrets" });
    const rotate = screen.getByRole("button", { name: "Rotate ANTHROPIC_KEY (global)" });
    expect(rotate.textContent).toBe("Rotate");
    const del = screen.getByRole("button", { name: "Delete OVH_TOKEN (project)" });
    expect(del.textContent).toBe("Delete");
  });

  it("leaves no two row controls sharing an accessible name", async () => {
    const container = mountSecrets();
    await screen.findByRole("table", { name: "Local secrets" });
    expectUniqueAccessibleNames(container, "Secrets · rows", {
      roles: [{ role: "button", matching: [/^Rotate /, /^Delete /] }],
      count: 6,
    });
  });
});

describe("local secrets axe", () => {
  it("Secrets — loaded table, axe smoke, incompletes included", async () => {
    const container = mountSecrets();
    // The surface has to have RENDERED before axe judges it: an entry over a
    // half-mounted tree is a verdict on nothing.
    await screen.findByRole("table", { name: "Local secrets" });
    await expectNoViolationsStrict(container, "Secrets");
  });

  it("Secrets — empty state", async () => {
    listLocalSecrets.mockResolvedValue([]);
    const container = mountSecrets();
    await screen.findByText("No secrets yet");
    await expectNoViolationsStrict(container, "Secrets (empty)");
  });
});
