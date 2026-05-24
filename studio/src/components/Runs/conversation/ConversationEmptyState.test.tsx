// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import type { RunStatus } from "@/api/runs";

import ConversationEmptyState from "./ConversationEmptyState";

afterEach(cleanup);

// ConversationEmptyState is the single source of truth for the
// empty-conversation pane (it replaced the dead `emptyCopy()` helper).
// These tests lock the per-status copy the operator actually sees so a
// future edit can't silently regress the lifecycle wording.
function renderState(
  status: RunStatus | undefined,
  currentRunStart?: string,
  onShowEventLog = () => {},
) {
  return render(
    <ConversationEmptyState
      status={status}
      currentRunStart={currentRunStart}
      onShowEventLog={onShowEventLog}
    />,
  );
}

describe("ConversationEmptyState", () => {
  it("explains the wait when the run is queued", () => {
    renderState("queued");
    expect(screen.getByText("Run queued")).toBeTruthy();
    expect(screen.getByText(/Waiting for a runner to pick it up/i)).toBeTruthy();
  });

  it("acknowledges active runs that haven't emitted output yet", () => {
    renderState("running");
    expect(screen.getByText("Run started")).toBeTruthy();
    expect(screen.getByText(/Waiting for the first agent output/i)).toBeTruthy();
  });

  it("surfaces a stalled hint + event-log link once a running run goes quiet", () => {
    // A start anchor well past the 30s threshold trips the stalled timer
    // synchronously inside render's act() wrapper.
    renderState("running", new Date(Date.now() - 60_000).toISOString());
    expect(
      screen.getByText(/running for a while without producing/i),
    ).toBeTruthy();
    expect(screen.getByText(/Show event log/i)).toBeTruthy();
  });

  it("makes the finished-but-empty case explicit", () => {
    renderState("finished");
    expect(screen.getByText("Run finished")).toBeTruthy();
    expect(
      screen.getByText(/No conversational messages were produced/i),
    ).toBeTruthy();
  });

  it("gives failed empty runs an actionable event-log affordance", () => {
    const onShowEventLog = vi.fn();

    renderState("failed", undefined, onShowEventLog);

    expect(screen.getByText("Run failed")).toBeTruthy();
    expect(screen.getByText(/Check the Events tab for error details/i)).toBeTruthy();
    const eventLogButton = screen.getByText(/Show event log/i);
    expect(eventLogButton).toBeTruthy();

    fireEvent.click(eventLogButton);
    expect(onShowEventLog).toHaveBeenCalledTimes(1);
  });

  it("tells operators resumable failed runs can resume after inspection", () => {
    const onShowEventLog = vi.fn();

    renderState("failed_resumable", undefined, onShowEventLog);

    expect(screen.getByText("Run failed")).toBeTruthy();
    expect(screen.getByText(/Check the Events tab for error details/i)).toBeTruthy();
    expect(screen.getByText(/use Resume when ready/i)).toBeTruthy();
    const eventLogButton = screen.getByText(/Show event log/i);
    expect(eventLogButton).toBeTruthy();

    fireEvent.click(eventLogButton);
    expect(onShowEventLog).toHaveBeenCalledTimes(1);
  });

  it("calls out the cancellation case so the silence isn't mysterious", () => {
    renderState("cancelled");
    expect(screen.getByText("Run cancelled")).toBeTruthy();
  });

  it("points to the form when paused waiting for human input", () => {
    renderState("paused_waiting_human");
    expect(screen.getByText("Run paused")).toBeTruthy();
    expect(screen.getByText(/Waiting for a human answer/i)).toBeTruthy();
  });

  it("points at the Resume button when paused by an operator", () => {
    renderState("paused_operator");
    expect(screen.getByText("Run paused by operator")).toBeTruthy();
    expect(screen.getByText(/Resume button in the header/i)).toBeTruthy();
  });

  it("falls back to the generic pre-flight line when status is unknown", () => {
    renderState(undefined);
    expect(screen.getByText(/Waiting for the run to start/i)).toBeTruthy();
  });
});
