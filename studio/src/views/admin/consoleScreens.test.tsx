// @vitest-environment jsdom
import { Suspense } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Route, Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";

// Every console screen, mounted through the console's own route table with the
// REAL pages — AdminConsoleRoutes.test.tsx replaces them with markers, which
// exercises the table and leaves what each page renders unexamined. Two claims
// this cluster makes hold for all twelve screens or for none:
//
//   - the screen offers the console nav, which is the only way off it;
//   - the per-page gate (copy-pasted into each page as defense-in-depth behind
//     RequireSuperAdmin) shows its notice INSTEAD of the screen.
//
// No data is ever delivered: every request stays pending, so each page renders
// its own loading layout. Both claims are decided before a fetch resolves, and
// a pending transport needs no per-screen payload — twelve response shapes
// kinder than the server's would prove less than this.

let mockUser: { is_super_admin?: boolean } | null = null;
let mockInfo: { mode?: string } | null = null;

vi.mock("@/auth/AuthContext", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/auth/AuthContext")>();
  // Only the identity: the module's predicates (hasRole, isLocalIdentity, …)
  // stay real, since the pages' own branches call them.
  const ctx = () => ({ user: mockUser, activeOrgID: "", reloadIdentity: async () => {} });
  return { ...actual, useAuth: ctx, useMaybeAuth: ctx };
});
vi.mock("@/store/serverInfo", () => ({
  useServerInfoStore: (sel: (s: { info: unknown }) => unknown) => sel({ info: mockInfo }),
}));

import AdminConsoleRoutes from "./AdminConsoleRoutes";
import { ADMIN_BASE, ADMIN_ITEMS } from "./adminRoutes";

const STUDIO_BASE = "/studio";
const CASES = ADMIN_ITEMS.map((item) => ({ path: item.path, label: item.label }));

// The console as App.tsx mounts it: <Router base="/studio"> then
// <Route path="/admin" nest>, minus RequireSuperAdmin — the only altitude at
// which the per-page gate is observable.
function mountConsole(path: string): void {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  const loc = memoryLocation({ path: `${STUDIO_BASE}${ADMIN_BASE}${path}` });
  render(
    <QueryClientProvider client={qc}>
      <Router hook={loc.hook} base={STUDIO_BASE}>
        <Route path={ADMIN_BASE} nest>
          <main>
            <Suspense fallback={<div>loading screen</div>}>
              <AdminConsoleRoutes />
            </Suspense>
          </main>
        </Route>
      </Router>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  // A request that never answers, so no screen depends on a payload shape to
  // render its chrome. Nothing here asserts on the transport.
  vi.stubGlobal("fetch", () => new Promise<Response>(() => {}));
  mockUser = { is_super_admin: true };
  mockInfo = { mode: "cloud" };
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  mockUser = null;
  mockInfo = null;
});

describe("the console's screens, through its route table", () => {
  it.each(CASES)("$label offers the console nav, the way off the screen", async ({ path }) => {
    mountConsole(path);
    expect(await screen.findByRole("navigation", { name: "Admin console" })).toBeTruthy();
    expect(screen.queryByText("Super-admin only.")).toBeNull();
  });

  it.each(CASES)("$label shows the super-admin notice to a non-super-admin", async ({ path }) => {
    mockUser = { is_super_admin: false };
    mountConsole(path);
    expect(await screen.findByText("Super-admin only.")).toBeTruthy();
    expect(screen.queryByRole("navigation", { name: "Admin console" })).toBeNull();
  });

  it.each(CASES)("$label degrades to the cloud-only notice in local mode", async ({ path }) => {
    mockInfo = { mode: "local" };
    mountConsole(path);
    expect(await screen.findByText(/cloud-mode feature/)).toBeTruthy();
    expect(screen.queryByRole("navigation", { name: "Admin console" })).toBeNull();
  });
});
