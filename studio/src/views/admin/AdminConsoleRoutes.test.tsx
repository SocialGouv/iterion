// @vitest-environment jsdom
import { Suspense } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { Route, Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";

// Each console screen is replaced by a marker so this exercises the route
// table and the Switch, not twelve page bodies (every page has its own tests).
vi.mock("@/views/admin/OrgsAdminPage", () => ({ default: () => <div>page:/orgs</div> }));
vi.mock("@/views/admin/UsersAdminPage", () => ({ default: () => <div>page:/users</div> }));
vi.mock("@/views/admin/AuditAdminPage", () => ({ default: () => <div>page:/audit</div> }));
vi.mock("@/views/admin/DLQAdminPage", () => ({ default: () => <div>page:/dlq</div> }));
vi.mock("@/views/admin/RunsAdminPage", () => ({ default: () => <div>page:/runs</div> }));
vi.mock("@/views/admin/PlatformLlmCredsPage", () => ({
  default: () => <div>page:/llm-credentials</div>,
}));
vi.mock("@/views/admin/PlatformBotsPage", () => ({ default: () => <div>page:/bots</div> }));
vi.mock("@/views/admin/PlatformForgeAppsPage", () => ({
  default: () => <div>page:/forge-apps</div>,
}));
vi.mock("@/views/admin/settings/UsageCapsPage", () => ({
  default: () => <div>page:/settings/usage-caps</div>,
}));
vi.mock("@/views/admin/settings/BotRolesPage", () => ({
  default: () => <div>page:/settings/bot-roles</div>,
}));
vi.mock("@/views/admin/settings/SandboxPage", () => ({
  default: () => <div>page:/settings/sandbox</div>,
}));
vi.mock("@/views/admin/settings/BotVarsPage", () => ({
  default: () => <div>page:/settings/bot-vars</div>,
}));
vi.mock("@/views/admin/settings/PlatformCredentialsPage", () => ({
  default: () => <div>page:/settings/platform-credentials</div>,
}));
vi.mock("@/views/admin/spend/CredentialSpendPage", () => ({
  default: () => <div>page:/spend</div>,
}));

import AdminConsoleRoutes from "./AdminConsoleRoutes";
import { ADMIN_BASE, ADMIN_HOME, ADMIN_PATHS } from "./adminRoutes";

const STUDIO_BASE = "/studio";

// The console's own mount: <Router base="/studio"> then
// <Route path="/admin" nest>, as App.tsx wires it.
function mountConsole(browserPath: string) {
  const loc = memoryLocation({ path: browserPath, record: true });
  render(
    <Router hook={loc.hook} base={STUDIO_BASE}>
      <Route path={ADMIN_BASE} nest>
        <Suspense fallback={<div>loading screen</div>}>
          <AdminConsoleRoutes />
        </Suspense>
      </Route>
    </Router>,
  );
  return loc;
}

afterEach(cleanup);

describe("AdminConsoleRoutes", () => {
  it.each([...ADMIN_PATHS])("mounts the %s console at its own path", async (path) => {
    mountConsole(`${STUDIO_BASE}${ADMIN_BASE}${path}`);
    expect(await screen.findByText(`page:${path}`)).toBeTruthy();
  });

  // The trailing-slash form of every console address answers: the server
  // redirects /admin/users/ to /studio/admin/users/ and keeps the slash
  // (registerStudioLegacyRedirects), so the SPA runs there. It must land on
  // the same screen, not on the catch-all.
  it.each([...ADMIN_PATHS])("mounts the %s console at its trailing-slash address", async (path) => {
    const loc = mountConsole(`${STUDIO_BASE}${ADMIN_BASE}${path}/`);
    expect(await screen.findByText(`page:${path}`)).toBeTruthy();
    expect(loc.history).toEqual([`${STUDIO_BASE}${ADMIN_BASE}${path}/`]);
  });

  // A case variant of a console address renders the screen too: wouter
  // compiles route patterns with the `i` flag and compares the base
  // case-insensitively, and the Go SPA handler serves index.html for any
  // unknown path (pkg/server/spa.go) so no redirect intervenes — measured
  // here through the history, which stays on the address as typed. The nav
  // has to credit the screen that rendered, like the trailing-slash form.
  it.each([...ADMIN_PATHS])("mounts the %s console at a case variant of its address", async (path) => {
    const loc = mountConsole(`${STUDIO_BASE}${ADMIN_BASE}${path.toUpperCase()}`);
    expect(await screen.findByText(`page:${path}`)).toBeTruthy();
    expect(loc.history).toEqual([`${STUDIO_BASE}${ADMIN_BASE}${path.toUpperCase()}`]);
  });

  // The WHOLE history, not its last entry: a push and a replace leave the
  // same last entry, so only this spelling holds `<Redirect replace>`.
  // Without it bare /admin stays behind, and Back re-enters the redirect.
  it("sends bare /admin to the console home instead of rendering nothing", async () => {
    const loc = mountConsole(`${STUDIO_BASE}${ADMIN_BASE}`);
    expect(await screen.findByText(`page:${ADMIN_HOME}`)).toBeTruthy();
    expect(loc.history).toEqual([`${STUDIO_BASE}${ADMIN_BASE}${ADMIN_HOME}`]);
  });

  it("sends a console path the table no longer declares to the console home", async () => {
    const loc = mountConsole(`${STUDIO_BASE}${ADMIN_BASE}/retired-screen`);
    expect(await screen.findByText(`page:${ADMIN_HOME}`)).toBeTruthy();
    expect(loc.history).toEqual([`${STUDIO_BASE}${ADMIN_BASE}${ADMIN_HOME}`]);
  });

  it("opens on a console the table declares, so the catch-all cannot loop", () => {
    expect(ADMIN_PATHS).toContain(ADMIN_HOME);
  });
});
