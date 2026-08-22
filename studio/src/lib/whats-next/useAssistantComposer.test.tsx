// @vitest-environment jsdom

import { act, renderHook } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { ASK_USER_RESPONSE_KEY } from "@/lib/askUserOptions";

import type { FirstClassBot } from "./firstClassBots";
import type { WhatsNextMessage } from "./messages";
import { useAssistantComposer } from "./useAssistantComposer";
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

  it("queues into a live run with the selected skills", async () => {
    const s = session();
    const { result } = renderHook(() => useAssistantComposer({ bot, session: s }));

    await act(() =>
      result.current.onComposerSend("inspect this", { skills: ["review"] }),
    );

    expect(api.queueMessage).toHaveBeenCalledWith("run-1", "inspect this", {
      skills: ["review"],
    });
    expect(s.launch).not.toHaveBeenCalled();
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
