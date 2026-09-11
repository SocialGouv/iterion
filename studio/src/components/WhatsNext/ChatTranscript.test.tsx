// @vitest-environment jsdom

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { WhatsNextMessage } from "@/lib/whats-next/messages";

import ChatTranscript from "./ChatTranscript";

function pending(id: string, prompt: string): WhatsNextMessage {
  return {
    kind: "human-question",
    id,
    nodeId: "chat",
    prompt,
    status: "pending",
  };
}

describe("ChatTranscript pending input ownership", () => {
  beforeEach(() => {
    Element.prototype.scrollIntoView = vi.fn();
  });

  afterEach(() => cleanup());

  it("keeps the questions visible while hiding stale inline forms behind the footer", () => {
    render(
      <ChatTranscript
        messages={[
          pending("chat:15:question", "The older gate"),
          pending("chat:38:question", "The current gate"),
        ]}
        footerOwnsPendingInput
        composerHandlesId="chat:38:question"
      />,
    );

    expect(screen.getByText("The older gate")).toBeTruthy();
    expect(screen.getByText("The current gate")).toBeTruthy();
    expect(screen.queryAllByPlaceholderText("Type your answer…")).toHaveLength(0);
  });

  it("leaves only the selected gate inline when no footer is mounted", () => {
    render(
      <ChatTranscript
        messages={[
          pending("chat:15:question", "The older gate"),
          pending("chat:38:question", "The current gate"),
        ]}
        composerHandlesId="chat:38:question"
      />,
    );

    expect(screen.queryAllByPlaceholderText("Type your answer…")).toHaveLength(1);
  });

  it("keeps the ordinary inline fallback when no shared owner is supplied", () => {
    render(<ChatTranscript messages={[pending("chat:1:question", "Reply")]} />);

    expect(screen.getByPlaceholderText("Type your answer…")).toBeTruthy();
  });

  it("renders a supplied slot after the last message without a question", () => {
    render(
      <ChatTranscript
        messages={[
          {
            kind: "assistant-text",
            id: "copi:1:txt:1",
            nodeId: "copi",
            iteration: 1,
            text: "The assistant is ready.",
          },
        ]}
        bubbleSlot={<span data-testid="assistant-action-slot">Confirm action</span>}
      />,
    );

    expect(screen.getByTestId("assistant-action-slot")).toBeTruthy();
  });

  it("keeps a supplied slot on the question row without duplicating it", () => {
    render(
      <ChatTranscript
        messages={[pending("chat:1:question", "Reply")]}
        bubbleSlot={<span data-testid="assistant-action-slot">Confirm action</span>}
      />,
    );

    expect(screen.getAllByTestId("assistant-action-slot")).toHaveLength(1);
  });

  it("does not add an empty fallback when no slot is supplied", () => {
    const { container } = render(
      <ChatTranscript
        messages={[
          {
            kind: "assistant-text",
            id: "copi:1:txt:1",
            nodeId: "copi",
            iteration: 1,
            text: "The assistant is ready.",
          },
        ]}
      />,
    );

    expect(container.querySelector("[data-testid='assistant-action-slot']")).toBeNull();
  });
});
