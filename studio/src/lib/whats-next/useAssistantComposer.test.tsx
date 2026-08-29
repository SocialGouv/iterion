// @vitest-environment jsdom

import { act, renderHook } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { ASK_USER_RESPONSE_KEY } from "@/lib/askUserOptions";

import type { FirstClassBot } from "./firstClassBots";
import type { WhatsNextMessage } from "./messages";
import { readQuickReplies, useAssistantComposer } from "./useAssistantComposer";
import type { UseWhatsNextSession } from "./useWhatsNextSession";

const api = vi.hoisted(() => ({ queueMessage: vi.fn() }));
vi.mock("@/api/queueMessages", () => api);

const bot: FirstClassBot = {
  id: "copilot",
  label: "Copi",
  description: "Assistant",
  workflowPath: "bots/copilot/main.bot",
  launcherVars: [],
  seedVar: "seed",
  nodeMap: { chat: { kind: "human", textField: "reply" } },
};

function session(
  patch: Partial<UseWhatsNextSession> = {},
): UseWhatsNextSession {
  return {
    status: "active",
    runId: "run-1",
    messages: [],
    busyMessageId: null,
    runStatus: "running",
    errorMessage: null,
    lastVars: { workspace_dir: "/repo" },
    discoveryError: null,
    retryDiscovery: vi.fn(),
    sessionRepo: null,
    launchRepo: null,
    modelPref: {} as UseWhatsNextSession["modelPref"],
    launch: vi.fn(),
    submitHumanAnswer: vi.fn(),
    newSession: vi.fn(),
    resume: vi.fn(),
    ...patch,
  };
}

function pending(questions?: Record<string, unknown>): WhatsNextMessage {
  return {
    kind: "human-question",
    id: "question-1",
    nodeId: "chat",
    prompt: "Reply",
    status: "pending",
    questions,
  };
}

describe("useAssistantComposer routing", () => {
  beforeEach(() => vi.resetAllMocks());

  it("answers a chat pause through the node's text field", async () => {
    const s = session({ messages: [pending()] });
    const { result } = renderHook(() =>
      useAssistantComposer({
        bot,
        session: s,
        decorate: (text) => `context\n${text}`,
      }),
    );

    await act(() => result.current.onComposerSend("  hello  ", { skills: [] }));

    expect(s.submitHumanAnswer).toHaveBeenCalledWith("question-1", {
      reply: "context\nhello",
    });
    expect(api.queueMessage).not.toHaveBeenCalled();
  });

  it("answers an ask_user pause through its reserved response field", async () => {
    const s = session({
      messages: [pending({ [ASK_USER_RESPONSE_KEY]: { type: "string" } })],
    });
    const { result } = renderHook(() => useAssistantComposer({ bot, session: s }));

    await act(() => result.current.onComposerSend("approve", { skills: [] }));

    expect(s.submitHumanAnswer).toHaveBeenCalledWith("question-1", {
      [ASK_USER_RESPONSE_KEY]: "approve",
    });
  });

  it("answers a custom delegate pause through its sole question key", async () => {
    const customBot = { ...bot, nodeMap: {} } as FirstClassBot;
    const s = session({
      runStatus: "paused_waiting_human",
      messages: [
        {
          kind: "human-question",
          id: "question-1",
          nodeId: "copi",
          prompt: "Reply",
          status: "pending",
          questions: {
            active_editor_document: "Send the active editor document.",
          },
        },
      ],
    });
    const { result } = renderHook(() =>
      useAssistantComposer({
        bot: customBot,
        session: s,
        decorate: (text) =>
          `<active-editor-document>{}</active-editor-document>\n${text}`,
      }),
    );

    await act(() =>
      result.current.onComposerSend("corrige le buffer", { skills: [] }),
    );

    expect(s.submitHumanAnswer).toHaveBeenCalledWith("question-1", {
      active_editor_document:
        "<active-editor-document>{}</active-editor-document>\ncorrige le buffer",
    });
    expect(api.queueMessage).not.toHaveBeenCalled();
  });

  it("submits an approval-only turn as a boolean under the declared field", async () => {
    const approvalBot: FirstClassBot = {
      ...bot,
      nodeMap: { chat: { kind: "human", approvedField: "accepted" } },
    };
    const s = session({ messages: [pending()] });
    const { result } = renderHook(() =>
      useAssistantComposer({ bot: approvalBot, session: s }),
    );

    expect(result.current.pendingApproval).toEqual({
      approvedField: "accepted",
    });
    await act(() => result.current.submitApproval(false));

    expect(s.submitHumanAnswer).toHaveBeenCalledWith("question-1", {
      accepted: false,
    });
  });

  it("submits hybrid approval and text under both declared fields", async () => {
    const approvalBot: FirstClassBot = {
      ...bot,
      nodeMap: {
        chat: {
          kind: "human",
          approvedField: "is_approved",
          textField: "revision_note",
        },
      },
    };
    const s = session({ messages: [pending()] });
    const { result } = renderHook(() =>
      useAssistantComposer({ bot: approvalBot, session: s }),
    );

    await act(() =>
      result.current.submitApproval(false, "  tighten the summary  "),
    );

    expect(s.submitHumanAnswer).toHaveBeenCalledWith("question-1", {
      is_approved: false,
      revision_note: "tighten the summary",
    });
  });

  it("does not decorate a constrained ask_user option", async () => {
    const s = session({
      messages: [
        pending({
          [ASK_USER_RESPONSE_KEY]: { type: "string" },
          _ask_user_options: [{ id: "approve", label: "Approve" }],
        }),
      ],
    });
    const { result } = renderHook(() =>
      useAssistantComposer({
        bot,
        session: s,
        decorate: (text) => `[page context: run/123]\n${text}`,
      }),
    );

    await act(() => result.current.onComposerSend("approve", { skills: [] }));

    expect(s.submitHumanAnswer).toHaveBeenCalledWith("question-1", {
      [ASK_USER_RESPONSE_KEY]: "approve",
    });
    expect(result.current.willDecorateMessage).toBe(false);
  });

  it("decorates a free-form ask_user answer", async () => {
    const s = session({
      messages: [pending({ [ASK_USER_RESPONSE_KEY]: { type: "string" } })],
    });
    const { result } = renderHook(() =>
      useAssistantComposer({
        bot,
        session: s,
        decorate: (text) => `[attached: run/123]\n${text}`,
      }),
    );

    await act(() => result.current.onComposerSend("investigate", { skills: [] }));

    expect(s.submitHumanAnswer).toHaveBeenCalledWith("question-1", {
      [ASK_USER_RESPONSE_KEY]: "[attached: run/123]\ninvestigate",
    });
    expect(result.current.willDecorateMessage).toBe(true);
  });

  it("awaits an asynchronous live-editor decorator before sending", async () => {
    const s = session();
    const { result } = renderHook(() =>
      useAssistantComposer({
        bot,
        session: s,
        decorate: async (text) => `<active-editor-document>{}</active-editor-document>\n${text}`,
      }),
    );

    await act(() => result.current.onComposerSend("change it", { skills: [] }));

    expect(api.queueMessage).toHaveBeenCalledWith(
      "run-1",
      "<active-editor-document>{}</active-editor-document>\nchange it",
      { skills: [] },
    );
  });

  it("queues into a live run with the selected skills", async () => {
    const s = session();
    const { result } = renderHook(() =>
      useAssistantComposer({ bot, session: s }),
    );

    await act(() =>
      result.current.onComposerSend("inspect this", { skills: ["review"] }),
    );

    expect(api.queueMessage).toHaveBeenCalledWith("run-1", "inspect this", {
      skills: ["review"],
    });
    expect(s.launch).not.toHaveBeenCalled();
  });

  it("never buffers an answer behind an unresolved human pause", async () => {
    const s = session({ runStatus: "paused_waiting_human", messages: [] });
    const { result } = renderHook(() => useAssistantComposer({ bot, session: s }));

    await act(async () => {
      await expect(
        result.current.onComposerSend("do not strand this", { skills: [] }),
      ).rejects.toThrow("waiting for a direct answer");
    });

    expect(api.queueMessage).not.toHaveBeenCalled();
  });

  it("re-seeds a closed session without dropping its scope vars", async () => {
    const s = session({ runStatus: "finished" });
    const { result } = renderHook(() => useAssistantComposer({ bot, session: s }));

    await act(() => result.current.onComposerSend("start over", { skills: [] }));

    expect(s.launch).toHaveBeenCalledWith({
      workspace_dir: "/repo",
      seed: "start over",
    });
    expect(api.queueMessage).not.toHaveBeenCalled();
  });

  it("rejects a send while discovery or launch is still resolving", async () => {
    const s = session({ status: "launching", runId: null, runStatus: null });
    const { result } = renderHook(() => useAssistantComposer({ bot, session: s }));

    await act(async () => {
      await expect(
        result.current.onComposerSend("do not duplicate", { skills: [] }),
      ).rejects.toThrow("still starting");
    });
    expect(s.launch).not.toHaveBeenCalled();
    expect(api.queueMessage).not.toHaveBeenCalled();
  });
});

describe("readQuickReplies", () => {
  it("reads typed navigate-then-send replies without treating the reference as an href", () => {
    expect(
      readQuickReplies({
        quick_replies: [
          {
            label: "Modifier ce bot",
            message: "Modifie le bot ouvert selon notre discussion.",
            navigate_to: "bot/bots/demo/main.bot",
          },
        ],
      }),
    ).toEqual([
      {
        label: "Modifier ce bot",
        message: "Modifie le bot ouvert selon notre discussion.",
        navigateTo: "bot/bots/demo/main.bot",
        legacy: false,
      },
    ]);
  });

  it("keeps old string replies usable while existing conversations drain", () => {
    expect(readQuickReplies({ quick_replies: '["Modifier le bot"]' })).toEqual([
      {
        label: "Modifier le bot",
        message: "Modifier le bot",
        navigateTo: null,
        legacy: true,
      },
    ]);
  });

  it("drops malformed typed replies", () => {
    expect(
      readQuickReplies({
        quick_replies: [
          { label: "No message", navigate_to: "view/editor" },
          { message: "" },
          42,
        ],
      }),
    ).toEqual([]);
  });
});
