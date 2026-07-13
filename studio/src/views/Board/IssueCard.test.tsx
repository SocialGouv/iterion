// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { IssueCard } from "./IssueCard";
import type { NativeIssue } from "@/api/native";

// PushToForgeButton pulls in AuthContext + server-info; the badge under test
// is independent of it, so stub it to a no-op.
vi.mock("./PushToForge", () => ({ PushToForgeButton: () => null }));

const baseIssue: NativeIssue = {
  id: "native:abc12345",
  title: "needs a decision",
  state: "in_progress",
  created_at: "2026-07-13T10:00:00Z",
  updated_at: "2026-07-13T10:00:00Z",
};

function renderCard(iss: NativeIssue) {
  return render(
    <IssueCard
      iss={iss}
      selected={false}
      activeLabels={new Set()}
      onClick={() => {}}
      onOpen={() => {}}
      onDragStart={() => {}}
      onLabelClick={() => {}}
      onCancelRun={() => {}}
      onOpenRun={() => {}}
      onShowRetryDetails={() => {}}
    />,
  );
}

afterEach(cleanup);

describe("IssueCard awaiting-input badge", () => {
  it("renders the ⏸ Awaiting input badge when awaiting_input is set", () => {
    renderCard({ ...baseIssue, awaiting_input: true });
    expect(screen.getByText(/Awaiting input/i)).toBeTruthy();
  });

  it("omits the badge when awaiting_input is falsy", () => {
    renderCard({ ...baseIssue, awaiting_input: false });
    expect(screen.queryByText(/Awaiting input/i)).toBeNull();
    renderCard(baseIssue); // undefined
    expect(screen.queryByText(/Awaiting input/i)).toBeNull();
  });

  it("hides the badge while a live run is in flight (running takes over)", () => {
    render(
      <IssueCard
        iss={{ ...baseIssue, awaiting_input: true }}
        selected={false}
        running={{ run_id: "run-1", issue_id: baseIssue.id } as never}
        activeLabels={new Set()}
        onClick={() => {}}
        onOpen={() => {}}
        onDragStart={() => {}}
        onLabelClick={() => {}}
        onCancelRun={() => {}}
        onOpenRun={() => {}}
        onShowRetryDetails={() => {}}
      />,
    );
    expect(screen.queryByText(/Awaiting input/i)).toBeNull();
  });
});
