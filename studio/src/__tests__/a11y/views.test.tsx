// @vitest-environment jsdom
import type { ReactNode } from "react";
import { afterEach, beforeAll, describe, it, vi } from "vitest";
import { cleanup, render } from "@testing-library/react";
import { Route, Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";

// Composed-surface a11y layer — extends the primitives axe smoke
// (primitives.test.tsx) up to whole panels/dialogs, where landmark
// conflicts, heading order and label associations actually surface. We
// only mount surfaces that render under jsdom with light setup (Zustand
// stores have defaults; no network). Canvas (xyflow) + WS-driven views
// stay manual / Playwright.

import EventLog from "@/components/Runs/EventLog";
import { EventsTab } from "@/components/Runs/detail/EventsTab";
import LibraryPanel from "@/components/Library/LibraryPanel";
import AppearanceTab from "@/views/SettingsDialog/AppearanceTab";
import AboutTab from "@/views/SettingsDialog/AboutTab";
import AuditTab from "@/views/teams/tabs/AuditTab";
import { RunViewSkeleton, RunViewLoadError } from "@/components/Runs/runView/RunViewLoadStates";
import AdminNav from "@/views/admin/AdminNav";
import { ADMIN_BASE } from "@/views/admin/adminRoutes";
import { setupMatchMedia, expectNoViolationsStrict } from "./axeHelpers";

// jsdom lacks matchMedia; the theme store + a few hooks read it on mount.
setupMatchMedia();

beforeAll(() => {
  // EventLog's list is a Virtuoso window; jsdom lacks ResizeObserver.
  vi.stubGlobal(
    "ResizeObserver",
    class {
      observe() {}
      unobserve() {}
      disconnect() {}
    },
  );
});

function mount(node: ReactNode): HTMLElement {
  // Wrap in <main> so single-panel surfaces sit in a landmark (axe's
  // region rule otherwise flags top-level content as not contained). NB: a
  // surface that renders its OWN <main> would be a nested-landmark bug
  // masked here — render such a surface unwrapped, or inside a <div>.
  return render(<main>{node}</main>).container;
}

describe("a11y / composed surfaces", () => {
  afterEach(() => cleanup());

  it("Settings · AppearanceTab — theme + chat-input radio groups", async () => {
    await expectNoViolationsStrict(mount(<AppearanceTab />), "AppearanceTab");
  });

  it("Settings · AboutTab — version/info panel", async () => {
    await expectNoViolationsStrict(mount(<AboutTab desktopFeatures={false} />), "AboutTab");
  });

  it("Teams · AuditTab — admin-only gate (no network)", async () => {
    // canManage=false short-circuits to a pure EmptyState before any
    // fetch, so this exercises the tab's landmark/label wiring under jsdom.
    await expectNoViolationsStrict(
      mount(<AuditTab teamID="team-1" canManage={false} />),
      "AuditTab",
    );
  });

  it("Admin · AdminNav — sectioned console nav, axe smoke", async () => {
    // Mounted the way the console mounts it: inside the studio base and the
    // nested /admin route, which is what decides its hrefs.
    //
    // axe only: it reports a dangling aria-labelledby as *incomplete* and
    // expectNoViolationsStrict reads violations only, so the section naming and the
    // current entry are pinned by AdminNav.test.tsx, not here.
    const loc = memoryLocation({ path: `/studio${ADMIN_BASE}/users` });
    await expectNoViolationsStrict(
      mount(
        <Router hook={loc.hook} base="/studio">
          <Route path={ADMIN_BASE} nest>
            <AdminNav />
          </Route>
        </Router>,
      ),
      "AdminNav",
    );
  });

  it("RunView · skeleton load state", async () => {
    await expectNoViolationsStrict(mount(<RunViewSkeleton />), "RunViewSkeleton");
  });

  it("RunView · error load states (404 + generic)", async () => {
    await expectNoViolationsStrict(
      mount(
        <div>
          <RunViewLoadError
            runId="run-abc"
            status={404}
            message="not found"
            onRetry={() => {}}
          />
          <RunViewLoadError
            runId="run-def"
            status={500}
            message="boom"
            onRetry={() => {}}
          />
        </div>,
      ),
      "RunViewLoadError",
    );
  });

  it("Runs · EventLog — the console toolbar, axe smoke", async () => {
    await expectNoViolationsStrict(
      mount(
        <EventLog
          events={[]}
          selectedExecutionId={null}
          followTail={false}
          onToggleFollow={() => {}}
        />,
      ),
      "EventLog",
    );
  });

  it("Runs · EventsTab — the detail tab toolbar, axe smoke", async () => {
    await expectNoViolationsStrict(mount(<EventsTab events={[]} />), "EventsTab");
  });

  it("Library · LibraryPanel — the side panel, axe smoke", async () => {
    await expectNoViolationsStrict(mount(<LibraryPanel />), "LibraryPanel");
  });
});
