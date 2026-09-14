// @vitest-environment jsdom
//
// Two lanes, one session host. Nexie is the co-CTO of /whats-next and answers
// ONLY there; the dock that rides every other route is the general iterion
// assistant. Before the split the two shared one selected bot, which was wrong
// in both directions: Nexie occupied the dock on /board by default, and
// selecting Copi made the "What's Next" tab answer as Copi.
//
// These tests pin the correspondent per route. The dock's own list is pinned
// in useChatRegistry.test.ts (resolveDockBot); this file is about the route.
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";

import { ASSISTANT_BOT_KEY } from "@/lib/chatDock/dockState";
import {
  readConversations,
  writeActiveConversation,
  writeConversations,
} from "@/lib/chatDock/conversations";
import type { UseWhatsNextSession } from "@/lib/whats-next/useWhatsNextSession";

import { AssistantProvider, useAssistantSession } from "./AssistantProvider";

const { nexie, copi, sessionState } = vi.hoisted(() => ({
  sessionState: {
    runId: null as string | null,
    session: {
      status: "idle" as const,
      runId: null as string | null,
      messages: [] as unknown[],
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
    },
  },
  nexie: {
    id: "whats-next",
    label: "Nexie",
    description: "",
    workflowPath: "bots/whats-next/main.bot",
    launcherVars: [],
    nodeMap: {},
  },
  copi: {
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
    byId: { "whats-next": nexie, copilot: copi },
    bots: [nexie, copi],
    dockBots: [copi],
    resolve: (id: string) => (id === "whats-next" ? nexie : copi),
    // Mirrors the real refusal: the dock never resolves to the /whats-next bot.
    resolveDock: () => copi,
    loading: false,
    error: null,
  }),
}));

vi.mock("@/lib/whats-next/useWhatsNextSession", () => ({
  useWhatsNextSession: () => sessionState.session as unknown as UseWhatsNextSession,
}));

function BotProbe() {
  const assistant = useAssistantSession();
  return <span data-testid="bot">{assistant?.bot?.label ?? "none"}</span>;
}

function DockListProbe() {
  const assistant = useAssistantSession();
  return (
    <span data-testid="dock-bots">
      {(assistant?.bots ?? []).map((b) => b.label).join(",")}
    </span>
  );
}

function renderAt(path: string, children: React.ReactNode) {
  window.history.pushState({}, "", path);
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <AssistantProvider>{children}</AssistantProvider>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  window.localStorage.clear();
  sessionState.runId = null;
  sessionState.session.runId = null;
});

afterEach(() => {
  cleanup();
  window.localStorage.clear();
  window.history.pushState({}, "", "/");
});

describe("the assistant's correspondent per route", () => {
  it("answers as the iterion assistant on an ordinary route", () => {
    renderAt("/board", <BotProbe />);
    expect(screen.getByTestId("bot").textContent).toBe("Copi");
  });

  it("answers as Nexie on her own tab", () => {
    renderAt("/whats-next", <BotProbe />);
    expect(screen.getByTestId("bot").textContent).toBe("Nexie");
  });

  // The inversion that made the split necessary: the operator picks Copi for
  // the dock, then opens What's Next and finds Copi answering there.
  it("keeps Nexie on her tab even when the dock is set to another bot", () => {
    window.localStorage.setItem(ASSISTANT_BOT_KEY, "copilot");
    renderAt("/whats-next", <BotProbe />);
    expect(screen.getByTestId("bot").textContent).toBe("Nexie");
  });

  // And the other direction: a selection naming Nexie must not drag her back
  // into the dock on an ordinary route.
  it("keeps Nexie out of the dock even when she is the persisted choice", () => {
    window.localStorage.setItem(ASSISTANT_BOT_KEY, "whats-next");
    renderAt("/runs", <BotProbe />);
    expect(screen.getByTestId("bot").textContent).toBe("Copi");
  });

  it("does not offer Nexie in the dock's own switcher", () => {
    renderAt("/board", <DockListProbe />);
    expect(screen.getByTestId("dock-bots").textContent).toBe("Copi");
  });

  // Nexie on /whats-next has her own store. Claiming her launch onto the
  // dock's active tab orphans that tab's run (and can cancel it on close).
  it("does not claim Nexie's launch onto the dock conversation", async () => {
    writeConversations([{ id: "dock-1", botId: "copilot", runId: "dock-run" }]);
    writeActiveConversation("dock-1");
    sessionState.runId = "nexie-run";
    sessionState.session.runId = "nexie-run";
    renderAt("/whats-next", <BotProbe />);
    await waitFor(() => {
      expect(screen.getByTestId("bot").textContent).toBe("Nexie");
    });
    expect(readConversations()[0]?.runId).toBe("dock-run");
  });

  it("does claim a launch onto the dock tab on an ordinary route", async () => {
    writeConversations([{ id: "dock-1", botId: "copilot" }]);
    writeActiveConversation("dock-1");
    sessionState.runId = "copi-run";
    sessionState.session.runId = "copi-run";
    renderAt("/board", <BotProbe />);
    await waitFor(() => {
      expect(readConversations()[0]?.runId).toBe("copi-run");
    });
  });
});
