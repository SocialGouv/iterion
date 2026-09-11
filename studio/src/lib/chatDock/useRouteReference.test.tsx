// @vitest-environment jsdom
// Route context reports only what is visible. Conversation-scoped opt-out and
// anchoring live in AssistantProvider so one tab cannot silence another.
import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { Router } from "wouter";
import { memoryLocation } from "wouter/memory-location";

import { useRouteReference } from "./useRouteReference";

afterEach(cleanup);

function Consumer() {
  const state = useRouteReference();
  return <span data-testid="reference">{state.reference?.ref ?? "none"}</span>;
}

function renderAt(path: string) {
  const { hook, navigate } = memoryLocation({ path });
  render(
    <Router hook={hook}>
      <Consumer />
    </Router>,
  );
  return navigate;
}

describe("useRouteReference", () => {
  it("tracks the visible route", () => {
    const navigate = renderAt("/board");
    expect(screen.getByTestId("reference").textContent).toBe("view/board");
    act(() => navigate("/runs/019f1234abcd"));
    expect(screen.getByTestId("reference").textContent).toBe(
      "run/019f1234abcd",
    );
  });

  it("reports no page pointer on the assistant's own route", () => {
    renderAt("/whats-next");
    expect(screen.getByTestId("reference").textContent).toBe("none");
  });
});
