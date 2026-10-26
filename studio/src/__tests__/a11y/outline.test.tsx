// @vitest-environment jsdom
import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { Route, Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";

import AppShell from "@/components/shared/AppShell";
import { AuthProvider } from "@/auth/AuthContext";
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

  it("keeps the banner landmark when the local-identity bar is empty", () => {
    // Local identity hides the account chip; with no page content pushed
    // the bar collapses to zero height but the landmark STAYS (RGAA 9.2:
    // one banner, always).
    const { container } = mount();
    expect(container.querySelector("header")).not.toBeNull();
  });
});
