// @vitest-environment jsdom
//
// The dismissal's lifetime, which is the part that is easy to get wrong:
// it has to outlive the DOCK (which unmounts on /whats-next, where the
// route renders the session itself) without outliving the REFERENCE
// (dismissing /board's chip must not silence /runs/:id's).
import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { Router, useLocation } from "wouter";
import { memoryLocation } from "wouter/memory-location";

import { AssistantProvider } from "@/components/ChatDock/AssistantProvider";

import { isAssistantOwnRoute } from "./routeReference";
import { useRouteReference } from "./useRouteReference";

const idleSession = vi.hoisted(() => ({
  status: "idle" as const,
  runId: null,
  messages: [],
  busyMessageId: null,
  runStatus: null,
  errorMessage: null,
  lastVars: null,
  discoveryError: null,
  retryDiscovery: () => {},
  sessionRepo: null,
  launchRepo: null,
  launch: async () => {},
  submitHumanAnswer: async () => {},
  newSession: () => {},
  resume: async () => {},
}));

vi.mock("@/lib/whats-next/useWhatsNextSession", () => ({
  useWhatsNextSession: () => idleSession,
}));

afterEach(cleanup);

// Stands in for the dock: mounted only on routes the dock renders on, so
// unmounting it is the /whats-next round trip.
let dismiss: () => void;
function Consumer() {
  const state = useRouteReference();
  dismiss = state.dismiss;
  return <span data-testid="active">{state.active?.ref ?? "none"}</span>;
}

function renderAt(path: string) {
  const { hook, navigate } = memoryLocation({ path });
  // AssistantProvider now discovers its registry through react-query (#333).
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const view = render(
    <QueryClientProvider client={qc}>
      <AssistantProvider>
        <Router hook={hook}>
          <Mounted />
        </Router>
      </AssistantProvider>
    </QueryClientProvider>,
  );
  return { navigate, view };
}

// The dock stands down on /whats-next (that route renders the session
// full-width), exactly as ChatDock does — same predicate, so the test
// unmounts for the same reason the app does.
function Mounted() {
  const [location] = useLocation();
  return isAssistantOwnRoute(location) ? null : <Consumer />;
}

const active = () => screen.queryByTestId("active")?.textContent ?? null;

describe("useRouteReference dismissal lifetime", () => {
  it("reports the route's reference until dismissed", () => {
    renderAt("/board");
    expect(active()).toBe("view/board");
    act(() => dismiss());
    expect(active()).toBe("none");
  });

  it("survives the dock unmounting on /whats-next", () => {
    const { navigate } = renderAt("/board");
    act(() => dismiss());
    expect(active()).toBe("none");

    // /whats-next: the dock is gone entirely.
    act(() => navigate("/whats-next"));
    expect(active()).toBeNull();

    // …and back. The dismissal was the operator's, not the mount's.
    act(() => navigate("/board"));
    expect(active()).toBe("none");
  });

  it("still lets another route contribute its own", () => {
    const { navigate } = renderAt("/board");
    act(() => dismiss());
    act(() => navigate("/runs/019f1234abcd"));
    expect(active()).toBe("run/019f1234abcd");
  });
});
