// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { Route, Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";

import AdminNav from "./AdminNav";
import {
  ADMIN_BASE,
  ADMIN_ITEMS,
  ADMIN_PATHS,
  ADMIN_SECTIONS,
  type AdminPath,
} from "./adminRoutes";

// App.tsx mounts the studio under <Router base="/studio"> and the console
// under <Route path="/admin" nest>. Inside that nest wouter reports a location
// relative to /admin and resolves every href against it, so a nav that holds
// absolute /admin/* paths highlights the wrong entry and sends each click to
// /admin/admin/*. Reproduce BOTH routers — the only place this nav ever runs.
const STUDIO_BASE = "/studio";

function browserPath(consolePath: string): string {
  return `${STUDIO_BASE}${ADMIN_BASE}${consolePath}`;
}

function mountNav(consolePath: string) {
  const loc = memoryLocation({ path: browserPath(consolePath), record: true });
  const { container } = render(
    <Router hook={loc.hook} base={STUDIO_BASE}>
      <Route path={ADMIN_BASE} nest>
        <AdminNav />
      </Route>
    </Router>,
  );
  return { loc, container };
}

function labelOf(path: AdminPath): string {
  const item = ADMIN_ITEMS.find((i) => i.path === path);
  if (!item) throw new Error(`no nav entry for ${path}`);
  return item.label;
}

// The console's URL surface, spelled out rather than derived: the nav and the
// route mounting share one table, so a test that only re-read it would follow a
// wrong table into production (and these URLs are what an operator bookmarks).
const HREFS: Record<string, string> = {
  Organizations: "/studio/admin/orgs",
  Users: "/studio/admin/users",
  "LLM credentials": "/studio/admin/llm-credentials",
  "Forge OAuth apps": "/studio/admin/forge-apps",
  "Usage caps": "/studio/admin/settings/usage-caps",
  "Bot roles": "/studio/admin/settings/bot-roles",
  Sandbox: "/studio/admin/settings/sandbox",
  "Bot vars": "/studio/admin/settings/bot-vars",
  "Platform creds": "/studio/admin/settings/platform-credentials",
  "Bot overrides": "/studio/admin/bots",
  Audit: "/studio/admin/audit",
  "Dead-letter queue": "/studio/admin/dlq",
  Spend: "/studio/admin/spend",
  Runs: "/studio/admin/runs",
};

afterEach(cleanup);

describe("AdminNav", () => {
  it("offers one link per console, each at its own URL", () => {
    mountNav("/orgs");
    expect(screen.getAllByRole("link").map((a) => a.textContent)).toEqual(
      Object.keys(HREFS),
    );
    for (const [label, href] of Object.entries(HREFS)) {
      expect(screen.getByRole("link", { name: label }).getAttribute("href")).toBe(href);
    }
    expect(Object.keys(HREFS)).toHaveLength(ADMIN_PATHS.length);
  });

  it("navigates to the console that was clicked, not to a doubled /admin path", () => {
    const { loc } = mountNav("/orgs");
    fireEvent.click(screen.getByRole("link", { name: "Audit" }));
    expect(loc.history).toEqual([browserPath("/orgs"), browserPath("/audit")]);
  });

  // Every spelling of a console address the route matcher accepts, since each
  // renders the screen: the trailing-slash form the server publishes
  // (/admin/users/ -> /studio/admin/users/) and the case variants wouter's
  // `i`-flagged patterns match (AdminConsoleRoutes.test.tsx measures both
  // renders). Whichever the operator lands on, the nav has to say which
  // console they are in.
  it("marks the console being viewed as the current page, and only it", () => {
    for (const path of ADMIN_PATHS) {
      for (const spelling of [path, `${path}/`, path.toUpperCase(), `${path.toUpperCase()}/`]) {
        const { container } = mountNav(spelling);
        const current = Array.from(container.querySelectorAll('[aria-current="page"]'));
        expect(current.map((el) => el.textContent), `at ${spelling}`).toEqual([
          labelOf(path),
        ]);
        cleanup();
      }
    }
  });

  it("groups the consoles into named sections, in the table's order", () => {
    mountNav("/orgs");
    expect(ADMIN_SECTIONS.map((s) => s.label)).toEqual(["Tenancy", "Platform", "Ops"]);
    for (const section of ADMIN_SECTIONS) {
      const list = screen.getByRole("list", { name: section.label });
      expect(within(list).getAllByRole("link").map((a) => a.textContent)).toEqual(
        section.items.map((i) => i.label),
      );
    }
  });
});
