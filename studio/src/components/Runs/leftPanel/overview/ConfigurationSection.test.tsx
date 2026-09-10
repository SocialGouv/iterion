// @vitest-environment jsdom

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { RunHeader, RunStatus } from "@/api/runs";

// Two vendor icon/UI barrels on this component's import path resolve
// their ESM subpaths in a way vitest cannot follow (@lobehub/ui via the
// shared chrome, @lobehub/icons via ProviderIcon). Both are pure
// presentation and neither is on the branch under test — stub them so
// the row-collection logic runs for real.
vi.mock("@/components/ui", () => ({
  Tooltip: ({ children }: { children: React.ReactNode }) => children,
}));
vi.mock("@/components/icons/ProviderIcon", () => ({
  ProviderIcon: () => null,
}));

import { ConfigurationSection } from "./ConfigurationSection";

afterEach(cleanup);

function mkRun(partial: Partial<RunHeader>): RunHeader {
  return {
    id: "run_x",
    workflow_name: "wf",
    status: "finished" as RunStatus,
    created_at: "2026-09-08T12:00:00Z",
    updated_at: "2026-09-08T12:00:00Z",
    active_duration_ms: 0,
    ...partial,
  } as RunHeader;
}

// The section is a collapsed <details>; jsdom still renders its children,
// so the rows are queryable without driving the disclosure.
function renderSection(run: RunHeader) {
  render(<ConfigurationSection run={run} />);
}

// #871 — the "Launched with" summary states which bundle served the run,
// and states NOTHING when the run recorded no tier. The defect this
// closes was an inert tier that every surface read as a working one, so
// the site that renders it must not manufacture an answer.
describe("ConfigurationSection — bundle tier", () => {
  it("names the team fork", () => {
    renderSection(mkRun({ bot_source_tier: "team", bot_source_tenant: "acme" }));
    expect(screen.getByText("Bundle")).toBeTruthy();
    expect(screen.getByText("team bot")).toBeTruthy();
  });

  it("states the baked catalog positively", () => {
    renderSection(mkRun({ bot_source_tier: "baked" }));
    expect(screen.getByText("baked catalog")).toBeTruthy();
  });

  it("renders no bundle row when no tier was recorded", () => {
    renderSection(mkRun({}));
    expect(screen.queryByText("Bundle")).toBeNull();
    // And in particular it does not fall back to the baked claim.
    expect(screen.queryByText("baked catalog")).toBeNull();
  });
});
