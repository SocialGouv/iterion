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
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useState } from "react";
import { Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";

import { ASSISTANT_DOCK_KEY } from "@/lib/chatDock/dockState";
import { readConversations } from "@/lib/chatDock/conversations";
import type { UseWhatsNextSession } from "@/lib/whats-next/useWhatsNextSession";

import { AssistantProvider } from "./AssistantProvider";
import ChatDock from "./ChatDock";

const retryDiscovery = vi.fn();
let session: UseWhatsNextSession;

// The real hook opens a websocket and lists runs on mount.
//
// Routable per engine: every conversation mounts its OWN session engine, so a
// single shared session cannot express the bug where one conversation's
// transcript is served under another conversation's id. `attachRunId` is what
// distinguishes them in production too — a conversation with a run attaches
// it, a freshly opened one has none. Unset by default, so the tests that only
// care about one conversation keep the shared `session`.
const sessionRoute = vi.hoisted(() => ({
  resolve: null as
    | ((attachRunId: string | null) => UseWhatsNextSession)
    | null,
}));
vi.mock("@/lib/whats-next/useWhatsNextSession", () => ({
  useWhatsNextSession: (
    _bot: unknown,
    opts?: { attachRunId?: string | null },
  ) => sessionRoute.resolve?.(opts?.attachRunId ?? null) ?? session,
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
    editor: { context: true },
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
  default: function MockComposer({
    onSend,
  }: {
    onSend: (text: string, opts: { skills: string[] }) => Promise<void>;
  }) {
    const [instance] = useState(() => ++composerInstances.next);
    return (
      <div data-testid="composer" data-instance={instance}>
        {instance}
        <button
          type="button"
          onClick={() =>
            void onSend(
              "Peux-tu me dire pourquoi le run #native:d5fc94b2-ca40-4cb5-9056-ea02fb8dfdaf a failed ?",
              { skills: [] },
            )
          }
        >
          send native reference
        </button>
      </div>
    );
  },
}));

const contextAPI = vi.hoisted(() => ({ resolve: vi.fn() }));
vi.mock("@/api/assistantContext", () => ({
  resolveAssistantContext: contextAPI.resolve,
}));

const editorAPI = vi.hoisted(() => ({ capture: vi.fn() }));
vi.mock("@/lib/chatDock/editorSession", () => ({
  captureActiveEditorDocument: editorAPI.capture,
}));

// One standing action request, so the offer tests below can tell "hidden by
// the pause gate" from "nothing to offer". Under the default `ask` policy the
// card renders a confirmation button and executes nothing.
const offered = vi.hoisted(() => ({
  request: {
    key: "run-1:copi:1:0",
    id: "pipeline.task.reset" as const,
    intent: "explicit" as const,
    args: { task_id: "native:d5fc94b2-ca40-4cb5-9056-ea02fb8dfdaf" },
  },
}));
vi.mock("@/hooks/useAssistantActions", () => ({
  useAssistantActions: () => [offered.request],
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
  contextAPI.resolve.mockReset();
  contextAPI.resolve.mockResolvedValue({ references: [] });
  editorAPI.capture.mockReset();
  editorAPI.capture.mockResolvedValue(null);
  composerInstances.next = 0;
  sessionRoute.resolve = null;
  // ChatTranscript scrolls its tail into view on mount and jsdom has no
  // layout: the throw would trip the assistant's ErrorBoundary and render the
  // app WITHOUT a dock, which makes every "the dock does not offer X"
  // assertion pass vacuously.
  Element.prototype.scrollIntoView = vi.fn();
  // The dock renders no body while closed; open it so the empty state
  // is on screen.
  window.localStorage.setItem(ASSISTANT_DOCK_KEY, "floating");
  session = makeSession();
});

afterEach(() => {
  cleanup();
  window.localStorage.clear();
});

function renderDock(path = "/") {
  // AssistantProvider discovers its bot registry through react-query now
  // (#333), so it needs a client. Retries off: the fetch fails under jsdom
  // and the registry's built-in floor is what these tests then exercise —
  // which is the production degradation path, not a stub.
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const location = memoryLocation({ path });
  const view = render(
    <QueryClientProvider client={qc}>
      <Router hook={location.hook}>
        <AssistantProvider>
          <ChatDock />
        </AssistantProvider>
      </Router>
    </QueryClientProvider>,
  );
  return { ...view, navigate: location.navigate };
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

describe("ChatDock active thinking status", () => {
  it("keeps Copi's textual thinking status visible when replay has no active node", () => {
    session = makeSession({
      status: "active",
      runId: "run-live",
      runStatus: "running",
    });
    renderDock();

    expect(screen.getByRole("status").textContent).toContain(
      "Copi is thinking",
    );
    expect(screen.queryByText(/conversation will start/i)).toBeNull();
  });

  it("does not duplicate the active node banner", () => {
    session = makeSession({
      status: "active",
      runId: "run-live",
      runStatus: "running",
      messages: [
        {
          kind: "banner",
          id: "copi:1",
          nodeId: "copi",
          label: "Copi is thinking",
          status: "running",
        },
      ],
    });
    renderDock();

    expect(screen.getAllByText("Copi is thinking")).toHaveLength(1);
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("does not claim Copi is thinking when a human question is open", () => {
    session = makeSession({
      status: "active",
      runId: "run-paused",
      runStatus: "running",
      messages: [
        {
          kind: "human-question",
          id: "chat:1:question",
          nodeId: "chat",
          prompt: "What should I do next?",
          status: "pending",
        },
      ],
    });
    renderDock();

    expect(screen.queryByText("Copi is thinking")).toBeNull();
  });
});

describe("ChatDock conversation isolation", () => {
  it("remounts conversation-owned draft state when opening another tab", () => {
    renderDock();
    expect(screen.getByTestId("composer").getAttribute("data-instance")).toBe("1");
    fireEvent.click(screen.getByRole("button", { name: /new conversation/i }));
    expect(screen.getByTestId("composer").getAttribute("data-instance")).toBe("2");
  });
});

describe("ChatDock host context resolution", () => {
  it("asks the host to resolve a native task typed on a generic page", async () => {
    const launch = vi.fn(async (_vars: Record<string, unknown>) => {});
    session = makeSession({ launch });
    contextAPI.resolve.mockResolvedValue({
      references: [
        {
          reference: "card/native:d5fc94b2-ca40-4cb5-9056-ea02fb8dfdaf",
          resolved: true,
          kind: "card",
          task: {
            id: "native:d5fc94b2-ca40-4cb5-9056-ea02fb8dfdaf",
            last_run_id: "01a044da-f33b-712c-b167-0b6ed6795c66",
          },
          run: {
            id: "01a044da-f33b-712c-b167-0b6ed6795c66",
            status: "cancelled",
          },
        },
      ],
    });
    renderDock();

    fireEvent.click(screen.getByRole("button", { name: "send native reference" }));

    await waitFor(() =>
      expect(contextAPI.resolve).toHaveBeenCalledWith(
        [],
        expect.stringContaining("#native:d5fc94b2-ca40-4cb5-9056-ea02fb8dfdaf"),
      ),
    );
    await waitFor(() => expect(launch).toHaveBeenCalled());
    const vars = launch.mock.calls[0]![0];
    expect(String(vars.initial_message)).toContain("<resolved-assistant-context>");
    expect(String(vars.initial_message)).toContain("01a044da-f33b-712c-b167-0b6ed6795c66");
  });
});

describe("ChatDock immutable conversation context", () => {
  it("injects implicit page context only into the first accepted message", async () => {
    const launch = vi.fn(async (_vars: Record<string, unknown>) => {});
    session = makeSession({ launch });
    renderDock("/board");

    fireEvent.click(screen.getByRole("button", { name: "send native reference" }));
    await waitFor(() => expect(launch).toHaveBeenCalledTimes(1));
    await waitFor(() =>
      expect(readConversations()[0]?.contextState).toBe("anchored"),
    );
    fireEvent.click(screen.getByRole("button", { name: "send native reference" }));
    await waitFor(() => expect(launch).toHaveBeenCalledTimes(2));

    const first = String(launch.mock.calls[0]![0].initial_message);
    const second = String(launch.mock.calls[1]![0].initial_message);
    expect(first).toContain("[page context: view/board]");
    expect(second).not.toContain("[page context:");
    expect(readConversations()[0]).toMatchObject({
      origin: "view/board",
      originHref: "/board",
      contextState: "anchored",
    });
  });

  it("anchors the page captured at click time when navigation wins the await", async () => {
    let release: ((value: { references: [] }) => void) | undefined;
    contextAPI.resolve.mockImplementationOnce(
      () =>
        new Promise<{ references: [] }>((resolve) => {
          release = resolve;
        }),
    );
    const launch = vi.fn(async (_vars: Record<string, unknown>) => {});
    session = makeSession({ launch });
    const { navigate } = renderDock("/board");

    fireEvent.click(screen.getByRole("button", { name: "send native reference" }));
    act(() => navigate("/runs/019f1234abcd"));
    await act(async () => release?.({ references: [] }));
    await waitFor(() => expect(launch).toHaveBeenCalledTimes(1));

    expect(String(launch.mock.calls[0]![0].initial_message)).toContain(
      "[page context: view/board]",
    );
    expect(readConversations()[0]).toMatchObject({
      origin: "view/board",
      originHref: "/board",
    });
  });

  it("lets a later page be joined explicitly without replacing the anchor", async () => {
    const launch = vi.fn(async (_vars: Record<string, unknown>) => {});
    session = makeSession({ launch });
    const { navigate } = renderDock("/board");
    fireEvent.click(screen.getByRole("button", { name: "send native reference" }));
    await waitFor(() => expect(launch).toHaveBeenCalledTimes(1));
    await waitFor(() => screen.getByText(/conversation context:/i));

    act(() => navigate("/runs/019f1234abcd"));
    fireEvent.click(await screen.findByRole("button", { name: "Join this page" }));
    fireEvent.click(screen.getByRole("button", { name: "send native reference" }));
    await waitFor(() => expect(launch).toHaveBeenCalledTimes(2));

    const second = String(launch.mock.calls[1]![0].initial_message);
    expect(second).toContain("[attached: run/019f1234abcd]");
    expect(second).not.toContain("[page context: run/019f1234abcd]");
    expect(readConversations()[0]?.origin).toBe("view/board");
  });

  it("keeps the anchored editor document live without repeating page context", async () => {
    editorAPI.capture.mockResolvedValue({
      sessionId: "editor-session",
      revision: 7,
      file: "bots/example/main.bot",
      complete: true,
      sourceLength: 12,
      source: "bot example",
    });
    const launch = vi.fn(async (_vars: Record<string, unknown>) => {});
    session = makeSession({ launch });
    renderDock("/editor?file=bots/example/main.bot");

    fireEvent.click(screen.getByRole("button", { name: "send native reference" }));
    await waitFor(() => expect(launch).toHaveBeenCalledTimes(1));
    fireEvent.click(screen.getByRole("button", { name: "send native reference" }));
    await waitFor(() => expect(launch).toHaveBeenCalledTimes(2));

    expect(editorAPI.capture).toHaveBeenCalledTimes(2);
    const second = String(launch.mock.calls[1]![0].initial_message);
    expect(second).toContain("<active-editor-document>");
    expect(second).not.toContain("[page context:");
  });

  it("migrates a legacy anchor from the opening transcript, not the current page", async () => {
    localStorage.setItem(
      "iterion.chatDock.conversations",
      JSON.stringify([
        {
          id: "legacy",
          botId: "copilot",
          runId: "run-legacy",
          origin: "view/board",
          originLabel: "Board",
        },
      ]),
    );
    session = makeSession({
      runId: "run-legacy",
      runStatus: "paused_waiting_human",
      messages: [
        {
          kind: "user-message",
          id: "seed",
          status: "consumed",
          text: "[page context: run/019f1234abcd]\n\nwhy did it fail?",
        },
      ],
    });
    renderDock("/pipelines");

    await waitFor(() =>
      expect(readConversations()[0]).toMatchObject({
        origin: "run/019f1234abcd",
        contextState: "anchored",
      }),
    );
  });

  it("recovers the opening anchor when a crash left a started tab pending", async () => {
    localStorage.setItem(
      "iterion.chatDock.conversations",
      JSON.stringify([
        {
          id: "interrupted",
          botId: "copilot",
          runId: "run-interrupted",
          contextState: "pending",
        },
      ]),
    );
    session = makeSession({
      runId: "run-interrupted",
      runStatus: "paused_waiting_human",
      messages: [
        {
          kind: "user-message",
          id: "seed",
          status: "consumed",
          text: "[page context: run/019f1234abcd]\n\nwhy did it fail?",
        },
      ],
    });
    renderDock("/pipelines");

    await waitFor(() =>
      expect(readConversations()[0]).toMatchObject({
        origin: "run/019f1234abcd",
        contextState: "anchored",
      }),
    );
  });

  // A conversation switch remounts the session ENGINE but not the state that
  // holds its published session, and the engine republishes from an effect —
  // so the previous conversation's transcript used to stay on the context for
  // at least one commit while `activeConversationId` already named the new
  // one. The migration effect read that foreign transcript as the new
  // conversation's own. Both outcomes below were observed on live runs.
  describe("does not inherit the transcript of the conversation it replaced", () => {
    function seedPredecessor(transcriptText: string) {
      localStorage.setItem(
        "iterion.chatDock.conversations",
        JSON.stringify([
          {
            id: "predecessor",
            botId: "copilot",
            runId: "run-predecessor",
            origin: "view/board",
            originLabel: "Board",
            originHref: "/board",
            contextState: "anchored",
          },
        ]),
      );
      const launch = vi.fn(async (_vars: Record<string, unknown>) => {});
      const predecessor = makeSession({
        runId: "run-predecessor",
        runStatus: "paused_waiting_human",
        messages: [
          {
            kind: "user-message",
            id: "seed",
            status: "consumed",
            text: transcriptText,
          },
        ],
      });
      // The fresh conversation has no run to attach: its own engine yields an
      // empty session, which is exactly why the stale one was visible.
      const successor = makeSession({ launch });
      sessionRoute.resolve = (attachRunId) =>
        attachRunId === "run-predecessor" ? predecessor : successor;
      return { launch };
    }

    function successorRecord() {
      return readConversations().find(
        (conversation) => conversation.id !== "predecessor",
      );
    }

    it("stays pending instead of being finalised unknown", async () => {
      seedPredecessor("why did it fail?");
      renderDock("/pipelines");

      fireEvent.click(screen.getByRole("button", { name: /new conversation/i }));
      await waitFor(() => expect(successorRecord()).toBeTruthy());
      // Let every effect the switch scheduled run before believing it.
      await act(async () => {});

      expect(successorRecord()?.contextState).toBe("pending");
      expect(successorRecord()?.origin).toBeUndefined();
    });

    // The worse half: the predecessor's transcript DOES carry a header, so the
    // effect recovered it and anchored the successor to a page it was never
    // opened from — `anchored`, therefore never revisited, and invisible.
    it("does not silently adopt the predecessor's anchor", async () => {
      seedPredecessor("[page context: view/board]\n\nwhy did it fail?");
      renderDock("/pipelines");

      fireEvent.click(screen.getByRole("button", { name: /new conversation/i }));
      await waitFor(() => expect(successorRecord()).toBeTruthy());
      await act(async () => {});

      expect(successorRecord()?.origin).toBeUndefined();
      expect(successorRecord()?.contextState).toBe("pending");
    });

    it("anchors its first message on the page it was actually opened from", async () => {
      const { launch } = seedPredecessor("why did it fail?");
      renderDock("/pipelines");

      fireEvent.click(screen.getByRole("button", { name: /new conversation/i }));
      await waitFor(() => expect(successorRecord()).toBeTruthy());
      await act(async () => {});
      fireEvent.click(screen.getByRole("button", { name: "send native reference" }));

      await waitFor(() => expect(launch).toHaveBeenCalledTimes(1));
      expect(String(launch.mock.calls[0]![0].initial_message)).toContain(
        "[page context: view/pipelines]",
      );
      await waitFor(() =>
        expect(successorRecord()).toMatchObject({
          origin: "view/pipelines",
          contextState: "anchored",
        }),
      );
    });
  });

  // Secondary defence for the same window. `runStatus` and the opening message
  // both come from one snapshot (the message is reconstructed from that
  // snapshot's launch var), so a non-null status over an empty transcript is a
  // run with nothing to read — not a transcript still in flight. Failing
  // towards `pending` keeps the conversation re-anchorable.
  it("does not finalise a conversation whose transcript is empty", async () => {
    localStorage.setItem(
      "iterion.chatDock.conversations",
      JSON.stringify([
        { id: "empty", botId: "copilot", runId: "run-empty" },
      ]),
    );
    session = makeSession({ runId: "run-empty", runStatus: "running" });
    renderDock("/pipelines");
    await act(async () => {});

    expect(readConversations()[0]?.contextState).toBeUndefined();
    expect(screen.getByText(/context for first message:/i)).toBeTruthy();
    expect(screen.getByText("Pipelines")).toBeTruthy();
    expect(screen.queryByText(/original context unavailable/i)).toBeNull();
  });

  // `unknown` used to be terminal: the effect returned before recovery could
  // run again. A conversation finalised while its transcript had not loaded
  // stayed stranded even once the header was on screen.
  it("recovers an unknown conversation once its opening header loads", async () => {
    localStorage.setItem(
      "iterion.chatDock.conversations",
      JSON.stringify([
        {
          id: "stranded",
          botId: "copilot",
          runId: "run-stranded",
          contextState: "unknown",
        },
      ]),
    );
    session = makeSession({
      runId: "run-stranded",
      runStatus: "paused_waiting_human",
      messages: [
        {
          kind: "user-message",
          id: "seed",
          status: "consumed",
          text: "[page context: view/board]\n\nwhy did it fail?",
        },
      ],
    });
    renderDock("/pipelines");

    await waitFor(() =>
      expect(readConversations()[0]).toMatchObject({
        origin: "view/board",
        contextState: "anchored",
      }),
    );
  });

  // The anchor stays what the first message carried. An unknown conversation
  // has no header to recover, so it is not re-anchored from wherever the
  // operator happens to be typing at turn seven.
  it("offers no page to join while the conversation has no anchor", async () => {
    localStorage.setItem(
      "iterion.chatDock.conversations",
      JSON.stringify([
        {
          id: "anchorless",
          botId: "copilot",
          runId: "run-anchorless",
          contextState: "unknown",
        },
      ]),
    );
    session = makeSession({
      runId: "run-anchorless",
      runStatus: "running",
      messages: [
        {
          kind: "user-message",
          id: "seed",
          status: "consumed",
          text: "why did it fail?",
        },
      ],
    });
    renderDock("/pipelines");
    await act(async () => {});

    expect(screen.queryByRole("button", { name: "Join this page" })).toBeNull();
    expect(readConversations()[0]?.contextState).toBe("unknown");
  });

  // `sameStudioHref` compares the query too, so the anchor page under a filter
  // read as a different page — the dock offered to join the page the operator
  // was standing on, and pointed "back" at it. Reference equality is the
  // second half of the same question, and both affordances now ask it.
  it("knows it is at the anchor when only the query differs", async () => {
    localStorage.setItem(
      "iterion.chatDock.conversations",
      JSON.stringify([
        {
          id: "filtered",
          botId: "copilot",
          runId: "run-filtered",
          origin: "view/pipelines",
          originLabel: "Pipelines",
          originHref: "/pipelines",
          contextState: "anchored",
        },
      ]),
    );
    session = makeSession({ runId: "run-filtered", runStatus: "running" });
    renderDock("/pipelines?state=running");
    await act(async () => {});

    expect(screen.queryByRole("button", { name: "Join this page" })).toBeNull();
    expect(screen.queryByRole("link", { name: /back to pipelines/i })).toBeNull();
  });

  it("keeps editor queries distinct in the anchor back link", async () => {
    localStorage.setItem(
      "iterion.chatDock.conversations",
      JSON.stringify([
        {
          id: "anchored",
          botId: "copilot",
          runId: "run-anchored",
          origin: "bot/bots/a/main.bot",
          originLabel: "bots/a/main.bot",
          originHref: "/editor?file=bots%2Fa%2Fmain.bot",
          contextState: "anchored",
        },
      ]),
    );
    session = makeSession({ runId: "run-anchored", runStatus: "running" });
    renderDock("/editor?file=bots%2Fb%2Fmain.bot");

    const link = await screen.findByRole("link", {
      name: /back to bots\/a\/main\.bot/i,
    });
    expect(link.getAttribute("href")).toBe(
      "/editor?file=bots%2Fa%2Fmain.bot",
    );
  });
});

// An offer belongs to a reply. The offers are built from the latest run
// artifact, which the first agent pass publishes BEFORE the optional private
// review/revision and before the chat node pauses — on run 01a04999 the action
// card was clickable while the editorial loop was still running. The dock must
// show it only once the turn is parked on its chat pause.
describe("ChatDock offers wait for the chat pause", () => {
  // ChatTranscript scrolls its tail into view on mount; jsdom has no layout
  // and no scrollIntoView, and the throw would trip the assistant's
  // ErrorBoundary into "the app without a dock" — an empty render that would
  // make the two "nothing offered" cases below pass vacuously.
  beforeEach(() => {
    Element.prototype.scrollIntoView = vi.fn();
  });

  const question = {
    kind: "human-question" as const,
    id: "chat:1:question",
    nodeId: "chat",
    prompt: "Je propose le reset de la tâche.",
  };

  it("offers nothing while the turn is still running (the reviewer's window)", () => {
    session = makeSession({
      status: "active",
      runId: "run-1",
      runStatus: "running",
      messages: [{ ...question, status: "answered", userReply: "ok go" }],
    });
    renderDock();
    // The transcript itself is on screen — the offer is gated, not the dock.
    expect(screen.getByText(/Je propose le reset/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Confirm action" })).toBeNull();
  });

  it("offers the action once the turn is parked on its chat pause", () => {
    session = makeSession({
      status: "active",
      runId: "run-1",
      runStatus: "paused_waiting_human",
      messages: [{ ...question, status: "pending" }],
    });
    renderDock();
    expect(screen.getByRole("button", { name: "Confirm action" })).toBeTruthy();
  });

  it("offers a published action when the chat pause has no question form", () => {
    session = makeSession({
      status: "active",
      runId: "run-1",
      runStatus: "paused_waiting_human",
      messages: [
        {
          kind: "assistant-text",
          id: "copi:1:txt:1",
          nodeId: "copi",
          iteration: 1,
          text: "La reprise est prête.",
        },
      ],
    });
    renderDock();
    expect(screen.getByRole("button", { name: "Confirm action" })).toBeTruthy();
  });

  it("offers nothing on a mid-turn ask_user pause (the artifact is last turn's)", () => {
    session = makeSession({
      status: "active",
      runId: "run-1",
      runStatus: "paused_waiting_human",
      messages: [
        {
          ...question,
          id: "ask-1",
          nodeId: "copi",
          status: "pending",
          questions: { ask_user_response: "Which task?" },
        },
      ],
    });
    renderDock();
    // The transcript itself is on screen — the offer is gated, not the dock.
    expect(screen.getByText(/Je propose le reset/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Confirm action" })).toBeNull();
  });
});
