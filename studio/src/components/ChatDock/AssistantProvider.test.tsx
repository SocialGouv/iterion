// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  ASSISTANT_DOCK_KEY,
  DOCK_BREAKPOINT_PX,
} from "@/lib/chatDock/dockState";
import { getDefaultRunStore, useRunStoreInstance } from "@/store/run";

import {
  AssistantProvider,
  AssistantStoreScope,
  useAssistantFixedInsetPx,
  useAssistantReservedWidthPx,
  useAssistantSession,
} from "./AssistantProvider";
import { DOCKED_WIDTH_PX, FLOATING_FOOTPRINT_PX } from "./ChatDockShell";

const { botLookup } = vi.hoisted(() => ({ botLookup: vi.fn() }));

// The registry is a server fetch now (manifest-driven discovery, #333), so
// what these tests need is the RESOLUTION, not the transport: mock the hook
// and keep asserting the one thing this file is about — that a registry with
// no usable bot degrades to "no assistant" instead of crashing the shell.
vi.mock("@/hooks/useChatRegistry", () => ({
  useChatRegistry: () => {
    const bot = botLookup();
    return {
      byId: bot ? { [bot.id]: bot } : {},
      bots: bot ? [bot] : [],
      dockBots: bot ? [bot] : [],
      resolve: () => bot,
      resolveDock: () => bot,
      loading: false,
      error: null,
    };
  },
}));

// The real hook opens a websocket and lists runs on mount; none of that
// is what this file is about.
vi.mock("@/lib/whats-next/useWhatsNextSession", () => ({
  useWhatsNextSession: () => ({
    status: "idle",
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
  }),
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
  localStorage.clear();
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
