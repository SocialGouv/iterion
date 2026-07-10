// @vitest-environment jsdom
import type { ReactNode } from "react";
import { afterEach, describe, it } from "vitest";
import { cleanup, render } from "@testing-library/react";

// Composed-surface a11y layer — extends the primitives axe smoke
// (primitives.test.tsx) up to whole panels/dialogs, where landmark
// conflicts, heading order and label associations actually surface. We
// only mount surfaces that render under jsdom with light setup (Zustand
// stores have defaults; no network). Canvas (xyflow) + WS-driven views
// stay manual / Playwright.

import AppearanceTab from "@/views/SettingsDialog/AppearanceTab";
import AboutTab from "@/views/SettingsDialog/AboutTab";
import AuditTab from "@/views/teams/tabs/AuditTab";
import { RunViewSkeleton, RunViewLoadError } from "@/components/Runs/runView/RunViewLoadStates";
import { setupMatchMedia, expectNoViolations } from "./axeHelpers";

// jsdom lacks matchMedia; the theme store + a few hooks read it on mount.
setupMatchMedia();

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
    await expectNoViolations(mount(<AppearanceTab />), "AppearanceTab");
  });

  it("Settings · AboutTab — version/info panel", async () => {
    await expectNoViolations(mount(<AboutTab desktopFeatures={false} />), "AboutTab");
  });

  it("Teams · AuditTab — admin-only gate (no network)", async () => {
    // canManage=false short-circuits to a pure EmptyState before any
    // fetch, so this exercises the tab's landmark/label wiring under jsdom.
    await expectNoViolations(
      mount(<AuditTab teamID="team-1" canManage={false} />),
      "AuditTab",
    );
  });

  it("RunView · skeleton load state", async () => {
    await expectNoViolations(mount(<RunViewSkeleton />), "RunViewSkeleton");
  });

  it("RunView · error load states (404 + generic)", async () => {
    await expectNoViolations(
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
});
