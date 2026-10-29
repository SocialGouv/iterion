// @vitest-environment jsdom
// RGAA 8.6 (#2516): every page's document.title names THAT page — the audit
// found 15 of 21 sampled routes sharing the identical "workspace · iterion
// studio" because the hook's if/else chain fell through to the bare project
// name. The titles now come from the route table (lib/routeTitles.ts); the
// guard in __tests__/a11y/route-titles.test.ts keeps table and routes in
// step, and this suite witnesses the composed titles themselves.
import { cleanup, render } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";

import { useDocumentTitle } from "./useDocumentTitle";
import { useServerInfoStore } from "@/store/serverInfo";

afterEach(cleanup);

function TitleProbe() {
  useDocumentTitle();
  return null;
}

function renderAt(path: string) {
  const { hook } = memoryLocation({ path });
  render(
    <Router hook={hook}>
      <TitleProbe />
    </Router>,
  );
}

beforeEach(() => {
  // The local-server shape the audit rendered: project named "workspace".
  useServerInfoStore.setState({
    info: { project_name: "workspace" } as never,
    loading: false,
  });
});

describe("useDocumentTitle — every page names itself (RGAA 8.6)", () => {
  it("titles each audited route after the page, not just the project", () => {
    const cases: [string, string][] = [
      ["/", "Home"],
      ["/whats-next", "What's Next"],
      ["/runs", "Runs"],
      ["/bots", "Bots"],
      ["/bots/new", "New bot"],
      ["/board", "Board"],
      ["/board/labels", "Board labels"],
      ["/board/fields", "Board fields"],
      ["/pipelines", "Pipelines"],
      ["/dispatcher", "Dispatcher"],
      ["/insights", "Insights"],
      ["/triggers", "Automations"],
      ["/marketplace", "Marketplace"],
      ["/plugins", "Plugins"],
      ["/skills", "Skills"],
      ["/secrets", "Secrets"],
      ["/sitemap", "Sitemap"],
    ];
    const seen = new Set<string>();
    for (const [path, page] of cases) {
      cleanup();
      renderAt(path);
      expect(document.title).toBe(`${page} — workspace · iterion studio`);
      expect(seen.has(document.title)).toBe(false);
      seen.add(document.title);
    }
  });

  it("names a bot home after the bot in the URL", () => {
    renderAt("/bots/featurly");
    expect(document.title).toBe("featurly — workspace · iterion studio");
  });

  it("keeps the launch route out of the run-detail branch", () => {
    renderAt("/runs/new");
    expect(document.title).toBe("Launch — workspace · iterion studio");
  });

  it("falls back to the static title while the run header loads", () => {
    renderAt("/runs/019f1234abcd");
    expect(document.title).toBe("Run — workspace · iterion studio");
  });

  it("titles the admin console from its own route table", () => {
    renderAt("/admin/users");
    expect(document.title).toBe("Admin — Users — workspace · iterion studio");
  });
});
