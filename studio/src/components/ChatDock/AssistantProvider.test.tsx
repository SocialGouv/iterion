// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  ASSISTANT_DOCK_KEY,
  ASSISTANT_DOCK_WIDTH_KEY,
  DOCK_BREAKPOINT_PX,
} from "@/lib/chatDock/dockState";
import {
  ACTIVE_CONVERSATION_KEY,
  CONVERSATIONS_KEY,
  readConversations,
} from "@/lib/chatDock/conversations";
import type { RunEvent } from "@/api/runs";
import { getDefaultRunStore, useRunStoreInstance } from "@/store/run";

import {
  AssistantProvider,
  AssistantStoreScope,
  useAssistantDock,
  useAssistantFixedInsetPx,
  useAssistantReservedWidthPx,
  useAssistantSession,
} from "./AssistantProvider";
import { DOCKED_WIDTH_PX, FLOATING_FOOTPRINT_PX } from "./ChatDockShell";

const { botLookup, registryLists } = vi.hoisted(() => ({
  botLookup: vi.fn(),
  registryLists: new WeakMap<object, object[]>(),
}));

const workspaceHandoffApi = vi.hoisted(() => ({
  redeemWorkspaceHandoff: vi.fn(),
  bindWorkspaceHandoff: vi.fn(),
}));

vi.mock("@/api/workspaceHandoff", () => workspaceHandoffApi);

// The registry is a server fetch now (manifest-driven discovery, #333), so
// what these tests need is the RESOLUTION, not the transport: mock the hook
// and keep asserting the one thing this file is about — that a registry with
// no usable bot degrades to "no assistant" instead of crashing the shell.
vi.mock("@/hooks/useChatRegistry", () => ({
  useChatRegistry: () => {
    const bot = botLookup();
    let bots: object[] = [];
    if (bot) {
      bots = registryLists.get(bot) ?? [bot];
      registryLists.set(bot, bots);
    }
    return {
      byId: bot ? { [bot.id]: bot } : {},
      bots,
      dockBots: bots,
      resolve: () => bot,
      resolveDock: () => bot,
      loading: false,
      error: null,
    };
  },
}));

// The real hook opens a websocket and lists runs on mount; none of that
// is what this file is about.
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

const sessionControl = vi.hoisted(() => ({
  calls: [] as Array<{ discover?: boolean; attachRunId?: string | null }>,
}));

vi.mock("@/lib/whats-next/useWhatsNextSession", () => ({
  // The real hook returns a fresh facade on every render. Preserve that
  // behavior here so a parent/engine publication loop fails this suite
  // instead of surfacing as an endless Suspense fallback in production.
  useWhatsNextSession: (
    _bot: unknown,
    options?: { discover?: boolean; attachRunId?: string | null },
  ) => {
    sessionControl.calls.push(options ?? {});
    return { ...idleSession };
  },
}));

const FAKE_BOT = {
  id: "whats-next",
  label: "Nexie",
  description: "",
  workflowPath: "bots/whats-next/main.bot",
  launcherVars: [],
  nodeMap: {},
};

beforeEach(() => {
  botLookup.mockReturnValue(FAKE_BOT);
  sessionControl.calls.length = 0;
  localStorage.clear();
  window.history.replaceState(null, "", "/");
  workspaceHandoffApi.redeemWorkspaceHandoff.mockReset();
  workspaceHandoffApi.bindWorkspaceHandoff.mockReset();
  workspaceHandoffApi.bindWorkspaceHandoff.mockResolvedValue({
    handoff_id: "handoff-1",
    bound: true,
  });
});

afterEach(cleanup);

// Reports which run store its position in the tree resolves to.
function Probe({ id }: { id: string }) {
  const store = useRunStoreInstance();
  return (
    <span data-testid={id}>
      {store === getDefaultRunStore() ? "default" : "assistant"}
    </span>
  );
}

describe("AssistantProvider run-store isolation", () => {
  // The whole point of giving the always-mounted session its own store:
  // on the default store it would permanently hold the assistant's run
  // for every shell-level consumer (useDocumentTitle would then title
  // /runs/:id after the assistant's run).
  it("hands the DEFAULT store to the app below it", () => {
    render(
      <AssistantProvider>
        <Probe id="route-tree" />
      </AssistantProvider>,
    );
    expect(screen.getByTestId("route-tree").textContent).toBe("default");
  });

  it("hands the ASSISTANT store to an AssistantStoreScope", () => {
    render(
      <AssistantProvider>
        <AssistantStoreScope>
          <Probe id="dock" />
        </AssistantStoreScope>
      </AssistantProvider>,
    );
    expect(screen.getByTestId("dock").textContent).toBe("assistant");
  });

  it("keeps both resolutions straight in one tree", () => {
    render(
      <AssistantProvider>
        <Probe id="outside" />
        <AssistantStoreScope>
          <Probe id="inside" />
        </AssistantStoreScope>
      </AssistantProvider>,
    );
    expect(screen.getByTestId("outside").textContent).toBe("default");
    expect(screen.getByTestId("inside").textContent).toBe("assistant");
  });

  // Outside the provider entirely (a surface rendered before the
  // authenticated shell), the scope must be inert rather than throw.
  it("is inert outside the provider", () => {
    render(
      <AssistantStoreScope>
        <Probe id="orphan" />
      </AssistantStoreScope>,
    );
    expect(screen.getByTestId("orphan").textContent).toBe("default");
  });
});

let appMounts = 0;

function AppMountMarker() {
  const [n] = useState(() => {
    appMounts += 1;
    return appMounts;
  });
  return <span data-testid="app-mounts">{n}</span>;
}

function OpenConversationButton() {
  const dock = useAssistantDock();
  return (
    <button type="button" data-testid="open-convo" onClick={() => dock?.openConversation()}>
      open
    </button>
  );
}

describe("the session key does not remount the app", () => {
  // ActiveConversation used to key the whole authenticated tree. Opening a
  // conversation (or hydrating the dock bot) remounted every route.
  it("keeps a child mounted when a new conversation is opened", () => {
    appMounts = 0;
    render(
      <AssistantProvider>
        <AppMountMarker />
        <OpenConversationButton />
      </AssistantProvider>,
    );
    expect(screen.getByTestId("app-mounts").textContent).toBe("1");
    fireEvent.click(screen.getByTestId("open-convo"));
    expect(screen.getByTestId("app-mounts").textContent).toBe("1");
  });
});

function ConversationStateProbe() {
  const dock = useAssistantDock();
  return (
    <>
      <span data-testid="waiting-conversations">
        {Array.from(dock?.waitingConversationIds ?? []).sort().join(",")}
      </span>
      <span data-testid="unread-watch-conversations">
        {Array.from(dock?.unreadWatchConversationIds ?? []).sort().join(",")}
      </span>
      <button
        type="button"
        onClick={() => {
          dock?.store.setState({
            runId: "assistant-run",
            events: [
              {
                seq: 65,
                timestamp: "2026-08-30T08:49:59Z",
                run_id: "assistant-run",
                node_id: "chat",
                type: "human_answers_recorded",
                data: {
                  answers: {
                    host_event: { kind: "assistant-watch-event" },
                  },
                },
              },
              {
                seq: 120,
                timestamp: "2026-08-30T08:50:59Z",
                run_id: "assistant-run",
                node_id: "chat",
                type: "human_input_requested",
                data: {},
              },
            ] satisfies RunEvent[],
          });
        }}
      >
        publish watch result
      </button>
      <button type="button" onClick={() => dock?.setDock("floating")}>
        open dock
      </button>
    </>
  );
}

describe("dock conversation runtime", () => {
  it("redeems a handoff into its own persisted project conversation", async () => {
    window.history.replaceState(null, "", "/?handoff=ticket-1");
    workspaceHandoffApi.redeemWorkspaceHandoff.mockResolvedValue({
      handoff_id: "handoff-1",
      source_project_id: "source-project",
      destination_project_id: "target-project",
      summary: "Update the shared bot",
      bot_id: "copilot",
    });

    render(
      <AssistantProvider>
        <ConversationStateProbe />
      </AssistantProvider>,
    );

    await waitFor(() =>
      expect(workspaceHandoffApi.redeemWorkspaceHandoff).toHaveBeenCalledTimes(1),
    );
    const [, clientId, conversationId] =
      workspaceHandoffApi.redeemWorkspaceHandoff.mock.calls[0] as string[];
    expect(clientId).toBeTruthy();
    expect(readConversations()).toContainEqual(
      expect.objectContaining({
        id: conversationId,
        workspaceHandoffId: "handoff-1",
        botId: "copilot",
      }),
    );
    expect(window.location.search).toBe("");
  });

  it("binds the target handoff only to the run owned by that conversation", async () => {
    localStorage.setItem(
      CONVERSATIONS_KEY,
      JSON.stringify([
        {
          id: "target-conversation",
          botId: "copilot",
          runId: "target-run",
          workspaceHandoffId: "handoff-1",
        },
      ]),
    );
    localStorage.setItem(ACTIVE_CONVERSATION_KEY, "target-conversation");

    render(
      <AssistantProvider>
        <ConversationStateProbe />
      </AssistantProvider>,
    );

    await waitFor(() =>
      expect(workspaceHandoffApi.bindWorkspaceHandoff).toHaveBeenCalledWith(
        "handoff-1",
        "target-run",
      ),
    );
  });

  it("never performs bot-scoped discovery for a dock tab without a run id", () => {
    localStorage.setItem(
      CONVERSATIONS_KEY,
      JSON.stringify([{ id: "legacy", botId: "copilot" }]),
    );

    render(
      <AssistantProvider>
        <ConversationStateProbe />
      </AssistantProvider>,
    );

    expect(sessionControl.calls).toEqual(
      expect.arrayContaining([
        expect.objectContaining({ discover: false, attachRunId: null }),
      ]),
    );
    expect(sessionControl.calls.some((call) => call.discover === true)).toBe(false);
  });

  it("keeps a watch diagnosis unread across a closed dock until its conversation opens", () => {
    localStorage.setItem(
      CONVERSATIONS_KEY,
      JSON.stringify([
        { id: "copi", botId: "copilot", runId: "assistant-run" },
      ]),
    );
    localStorage.setItem(ACTIVE_CONVERSATION_KEY, "copi");
    localStorage.setItem(ASSISTANT_DOCK_KEY, "closed");

    render(
      <AssistantProvider>
        <ConversationStateProbe />
      </AssistantProvider>,
    );

    fireEvent.click(screen.getByRole("button", { name: "publish watch result" }));
    expect(screen.getByTestId("unread-watch-conversations").textContent).toBe(
      "copi",
    );
    expect(readConversations()[0]?.lastReadWatchResultSeq).toBeUndefined();

    fireEvent.click(screen.getByRole("button", { name: "open dock" }));
    expect(screen.getByTestId("unread-watch-conversations").textContent).toBe("");
    expect(readConversations()[0]?.lastReadWatchResultSeq).toBe(120);
  });
});

// Reports both halves of the invariant from one render, so the test
// cannot pass by checking them under different conditions.
function WidthProbe() {
  const width = useAssistantReservedWidthPx();
  const session = useAssistantSession();
  return (
    <>
      <span data-testid="width">{width}</span>
      <span data-testid="renders">{session?.bot ? "yes" : "no"}</span>
    </>
  );
}

describe("useAssistantReservedWidthPx", () => {
  // AppShell turns this into right-edge padding and the run console's
  // steering bubble offsets by it, so it must agree with ChatDock's own
  // render guard: reserving a 380px column that nothing fills leaves a
  // dead band down the side of every page.
  it("reserves the column when the dock is docked and has a bot", () => {
    localStorage.setItem(ASSISTANT_DOCK_KEY, "docked-right");
    render(
      <AssistantProvider>
        <WidthProbe />
      </AssistantProvider>,
    );
    expect(screen.getByTestId("renders").textContent).toBe("yes");
    expect(screen.getByTestId("width").textContent).toBe(String(DOCKED_WIDTH_PX));
  });

  // The registry IS manifest-driven now, so a miss is reachable: a server
  // that serves no chat bot, or a listing still in flight on a cold load.
  it("reserves nothing when the bot lookup misses", () => {
    botLookup.mockReturnValue(null);
    localStorage.setItem(ASSISTANT_DOCK_KEY, "docked-right");
    render(
      <AssistantProvider>
        <WidthProbe />
      </AssistantProvider>,
    );
    expect(screen.getByTestId("renders").textContent).toBe("no");
    expect(screen.getByTestId("width").textContent).toBe("0");
  });

  it("reserves nothing while the dock is floating or closed", () => {
    localStorage.setItem(ASSISTANT_DOCK_KEY, "floating");
    render(
      <AssistantProvider>
        <WidthProbe />
      </AssistantProvider>,
    );
    expect(screen.getByTestId("width").textContent).toBe("0");
  });

  it("uses the dock as an overlay instead of squeezing compact screens", () => {
    const previous = window.innerWidth;
    Object.defineProperty(window, "innerWidth", {
      configurable: true,
      value: DOCK_BREAKPOINT_PX,
    });
    localStorage.setItem(ASSISTANT_DOCK_KEY, "docked-right");
    render(
      <AssistantProvider>
        <WidthProbe />
      </AssistantProvider>,
    );
    expect(screen.getByTestId("width").textContent).toBe("0");
    Object.defineProperty(window, "innerWidth", {
      configurable: true,
      value: DOCK_BREAKPOINT_PX + 1,
    });
    fireEvent(window, new Event("resize"));
    expect(screen.getByTestId("width").textContent).toBe(
      String(DOCKED_WIDTH_PX),
    );
    Object.defineProperty(window, "innerWidth", {
      configurable: true,
      value: previous,
    });
  });

  it("re-clamps the reserved width when a wide viewport narrows", () => {
    const previous = window.innerWidth;
    Object.defineProperty(window, "innerWidth", {
      configurable: true,
      value: 2000,
    });
    localStorage.setItem(ASSISTANT_DOCK_KEY, "docked-right");
    localStorage.setItem(ASSISTANT_DOCK_WIDTH_KEY, "1400");
    render(
      <AssistantProvider>
        <WidthProbe />
      </AssistantProvider>,
    );
    expect(screen.getByTestId("width").textContent).toBe("1400");

    Object.defineProperty(window, "innerWidth", {
      configurable: true,
      value: 1000,
    });
    fireEvent(window, new Event("resize"));
    expect(screen.getByTestId("width").textContent).toBe("700");

    Object.defineProperty(window, "innerWidth", {
      configurable: true,
      value: previous,
    });
  });
});

// The layout reservation and what a PEER fixed surface must clear are two
// different questions, and answering the second with the first is what made
// the run console's steering bubble unclickable: `fixed` elements ignore
// padding, so the bubble at right:80 sat UNDER a floating assistant spanning
// right 16 -> 436, with the same z-index and the assistant mounted later.
// `closed` is the persisted default for the steering panel, so this was the
// ordinary configuration on /runs/:id, not an exotic one.
function InsetProbe() {
  return <span data-testid="inset">{useAssistantFixedInsetPx()}</span>;
}

describe("useAssistantFixedInsetPx", () => {
  it("clears the docked column", () => {
    localStorage.setItem(ASSISTANT_DOCK_KEY, "docked-right");
    render(
      <AssistantProvider>
        <InsetProbe />
      </AssistantProvider>,
    );
    expect(screen.getByTestId("inset").textContent).toBe(
      String(DOCKED_WIDTH_PX),
    );
  });

  it("clears the FLOATING panel too, which the layout reservation does not", () => {
    localStorage.setItem(ASSISTANT_DOCK_KEY, "floating");
    render(
      <AssistantProvider>
        <InsetProbe />
      </AssistantProvider>,
    );
    expect(screen.getByTestId("inset").textContent).toBe(
      String(FLOATING_FOOTPRINT_PX),
    );
    expect(FLOATING_FOOTPRINT_PX).toBeGreaterThan(80); // the lane-1 bubble
  });

  it("clears nothing when the assistant is closed", () => {
    localStorage.setItem(ASSISTANT_DOCK_KEY, "closed");
    render(
      <AssistantProvider>
        <InsetProbe />
      </AssistantProvider>,
    );
    expect(screen.getByTestId("inset").textContent).toBe("0");
  });

  it("clears nothing when the bot lookup misses", () => {
    botLookup.mockReturnValue(null);
    localStorage.setItem(ASSISTANT_DOCK_KEY, "floating");
    render(
      <AssistantProvider>
        <InsetProbe />
      </AssistantProvider>,
    );
    expect(screen.getByTestId("inset").textContent).toBe("0");
  });
});
