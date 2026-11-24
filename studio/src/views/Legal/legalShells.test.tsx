// @vitest-environment jsdom
import { Suspense } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";

import type { ServerInfo } from "@/api/types";
import { useServerInfoStore } from "@/store/serverInfo";
import { setupMatchMedia } from "@/__tests__/a11y/axeHelpers";
import RestrictedShell from "@/views/RestrictedShell";
import ConfigEditorShell from "@/views/ConfigEditorShell";

// The two role shells have no sidebar, so they carry the legal links in
// their own foot (RGAA 12: every signed-in page reaches them; ADR-160) — and
// they render inside the studio's router base, so the links must still name
// the root address /legal/<slug>.

setupMatchMedia();

vi.mock("@/auth/AuthContext", () => ({
  useAuth: () => ({ user: { email: "r@example.test" }, signOut: vi.fn(), activeTeamID: null, activeTeam: null }),
}));
vi.mock("@/views/Marketplace", () => ({ default: () => <h1>Marketplace</h1> }));

const CONFIGURED = {
  mode: "cloud",
  legal: {
    contact_email: "contact@example.org",
    accessibility: { status: "non_compliant" },
    documents: [{ slug: "mentions-legales", title: "Mentions légales" }],
  },
} as ServerInfo;

afterEach(() => cleanup());

describe("the role shells carry the legal links", () => {
  for (const [name, Shell] of [
    ["RestrictedShell", RestrictedShell],
    ["ConfigEditorShell", ConfigEditorShell],
  ] as const) {
    it(`${name} links the configured documents at their root address`, async () => {
      useServerInfoStore.setState({ info: CONFIGURED });
      const loc = memoryLocation({ path: "/studio" });
      render(
        <Router hook={loc.hook}>
          <Router base="/studio">
            <Suspense fallback={null}>
              <Shell />
            </Suspense>
          </Router>
        </Router>,
      );
      const nav = await screen.findByRole("navigation", { name: "Informations légales" });
      const link = Array.from(nav.querySelectorAll("a")).find((a) => a.textContent === "Mentions légales");
      expect(link?.getAttribute("href")).toBe("/legal/mentions-legales");
    });
  }
});
