// @vitest-environment jsdom
import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { Route, Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";

import AppShell from "@/components/shared/AppShell";
import { AuthProvider } from "@/auth/AuthContext";
import { useUIStore } from "@/store/ui";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

afterEach(cleanup);

// The document-structure contract (RGAA 9.2): the authenticated shell
// exposes exactly one banner, one primary navigation, one main — the
// landmarks every later outline assertion in this file builds on.
describe("the shell's landmark outline", () => {
  function mount(path = "/runs") {
    const loc = memoryLocation({ path: `/studio${path}` });
    return render(
      <Router hook={loc.hook} base="/studio">
        <QueryClientProvider client={new QueryClient()}>
        <AuthProvider>
          <Route path="/runs">
            <AppShell>
              <div>route content</div>
            </AppShell>
          </Route>
        </AuthProvider>
        </QueryClientProvider>
      </Router>,
    );
  }

  it("exposes exactly one banner, one nav, one main", () => {
    const { container } = mount();
    expect(container.querySelectorAll("header").length).toBe(1);
    expect(container.querySelectorAll("main").length).toBe(1);
    expect(container.querySelectorAll("nav").length).toBe(1);
  });

  it("the primary navigation groups are real lists (RGAA 9.3)", () => {
    const { container } = mount();
    const nav = container.querySelector('nav[aria-label="Primary navigation"]')!;
    const lists = nav.querySelectorAll("ul");
    expect(lists.length).toBeGreaterThan(0);
    for (const ul of lists) {
      expect(ul.querySelectorAll("li").length).toBeGreaterThan(0);
    }
  });

  it("RunHeader's principal title is the run-detail h1 (RGAA 9.1, #2517)", () => {
    // Source pin: the full RunHeader mount drags the run store and the
    // websocket stack; the pin holds the wrapper mechanism at the source.
    const src = readFileSync(
      resolve(process.cwd(), "src/components/Runs/RunHeader.tsx"),
      "utf8",
    );
    expect(src).toContain("<h1 className=\"inline-flex items-center gap-1 min-w-0 group");
  });

  it("the runs table caption summarizes the columns and the grouping (RGAA 5.1, #2508)", () => {
    const src = readFileSync(
      resolve(process.cwd(), "src/components/Runs/RunListView.tsx"),
      "utf8",
    );
    expect(src).toContain("Runs by state group");
    expect(src).toContain("duration, run ID");
  });

  it("keeps the banner landmark when the local-identity bar is empty", () => {
    // Local identity hides the account chip; with no page content pushed
    // the bar collapses to zero height but the landmark STAYS (RGAA 9.2:
    // one banner, always).
    const { container } = mount();
    expect(container.querySelector("header")).not.toBeNull();
  });
});
