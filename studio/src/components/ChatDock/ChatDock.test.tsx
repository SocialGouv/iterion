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
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

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

// The composer talks to the run API on mount; irrelevant here and the
// dock renders it under every branch below.
vi.mock("@/components/shared/AgentChatboxInline", () => ({
  default: () => <div data-testid="composer" />,
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
  render(
    <AssistantProvider>
      <ChatDock />
    </AssistantProvider>,
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
