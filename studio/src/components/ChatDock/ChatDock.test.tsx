// @vitest-environment jsdom
//
// The dock's composer IS its launcher, and the dock is mounted on every
// authenticated route — so a failed startup discovery has to look
// different from "no session yet". Otherwise the operator's next
// keystroke launches a second Nexie over a live one, and the only
// surface that could have warned them showed an invitation instead.
//
// The sibling that gets this right is SessionLauncher on /whats-next;
// these tests pin the dock to the same behaviour so the two stop
// drifting.
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useState } from "react";

import { ASSISTANT_DOCK_KEY } from "@/lib/chatDock/dockState";
import type { UseWhatsNextSession } from "@/lib/whats-next/useWhatsNextSession";

import { AssistantProvider } from "./AssistantProvider";
import ChatDock from "./ChatDock";

const retryDiscovery = vi.fn();
let session: UseWhatsNextSession;

// The real hook opens a websocket and lists runs on mount.
vi.mock("@/lib/whats-next/useWhatsNextSession", () => ({
  useWhatsNextSession: () => session,
}));

// The dock hosts every chat bot EXCEPT the one that owns /whats-next, so the
// built-in floor (which is that bot alone) leaves it with no correspondent.
// These tests are about the dock's empty/degraded BODY, not about discovery,
// so give it one eligible bot; the "no eligible bot" path is pinned by its own
// test at the bottom of this file.
const { dockBot } = vi.hoisted(() => ({
  dockBot: {
    id: "copilot",
    label: "Copi",
    description: "",
    workflowPath: "bots/copilot/main.bot",
    launcherVars: [],
    nodeMap: {},
  },
}));

vi.mock("@/hooks/useChatRegistry", () => ({
  useChatRegistry: () => ({
    byId: { copilot: dockBot },
    bots: [dockBot],
    dockBots: [dockBot],
    resolve: () => dockBot,
    resolveDock: () => dockBot,
    loading: false,
    error: null,
  }),
}));

// The composer talks to the run API on mount; irrelevant here and the
// dock renders it under every branch below.
const composerInstances = vi.hoisted(() => ({ next: 0 }));
vi.mock("@/components/shared/AgentChatboxInline", () => ({
  default: function MockComposer() {
    const [instance] = useState(() => ++composerInstances.next);
    return <div data-testid="composer">{instance}</div>;
  },
}));

function makeSession(over: Partial<UseWhatsNextSession> = {}): UseWhatsNextSession {
  return {
    status: "idle",
    runId: null,
    messages: [],
    busyMessageId: null,
    runStatus: null,
    errorMessage: null,
    lastVars: null,
    discoveryError: null,
    retryDiscovery,
    sessionRepo: null,
    launchRepo: null,
    launch: async () => {},
    submitHumanAnswer: async () => {},
    newSession: () => {},
    resume: async () => {},
    ...over,
  } as UseWhatsNextSession;
}

beforeEach(() => {
  retryDiscovery.mockClear();
  composerInstances.next = 0;
  // The dock renders no body while closed; open it so the empty state
  // is on screen.
  window.localStorage.setItem(ASSISTANT_DOCK_KEY, "floating");
  session = makeSession();
});

afterEach(() => {
  cleanup();
  window.localStorage.clear();
});

function renderDock() {
  // AssistantProvider discovers its bot registry through react-query now
  // (#333), so it needs a client. Retries off: the fetch fails under jsdom
  // and the registry's built-in floor is what these tests then exercise —
  // which is the production degradation path, not a stub.
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <AssistantProvider>
        <ChatDock />
      </AssistantProvider>
    </QueryClientProvider>,
  );
}

describe("ChatDock empty state", () => {
  it("invites a first message when discovery found nothing", () => {
    renderDock();
    expect(screen.getByText(/the first message starts a session/i)).toBeTruthy();
    expect(screen.queryByRole("button", { name: /retry/i })).toBeNull();
  });

  it("warns instead of inviting when discovery FAILED", () => {
    session = makeSession({ discoveryError: "listRuns failed: 503" });
    renderDock();

    expect(screen.queryByText(/the first message starts a session/i)).toBeNull();
    expect(screen.getByText(/couldn't check for a running session/i)).toBeTruthy();
    // The reason and the stake, not just a shrug.
    expect(screen.getByText(/listRuns failed: 503/)).toBeTruthy();
    expect(screen.getByText(/two sessions in parallel/i)).toBeTruthy();
  });

  it("offers the session's own retry", () => {
    session = makeSession({ discoveryError: "network error" });
    renderDock();
    screen.getByRole("button", { name: /retry/i }).click();
    expect(retryDiscovery).toHaveBeenCalled();
  });
});

describe("ChatDock conversation isolation", () => {
  it("remounts conversation-owned draft state when opening another tab", () => {
    renderDock();
    expect(screen.getByTestId("composer").textContent).toBe("1");
    fireEvent.click(screen.getByRole("button", { name: /new conversation/i }));
    expect(screen.getByTestId("composer").textContent).toBe("2");
  });
});
