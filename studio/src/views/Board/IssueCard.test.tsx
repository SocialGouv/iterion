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

describe("IssueCard approval banner", () => {
  it("renders the approval banner + author when the card carries needs:approval", () => {
    renderCard({
      ...baseIssue,
      labels: ["bug", "needs:approval"],
      external: {
        provider: "github",
        connection_id: "c1",
        repo: "acme/widgets",
        number: 7,
        author: "drive-by",
      },
    });
    expect(screen.getByText(/approval required/i)).toBeTruthy();
    expect(screen.getByText(/@drive-by/)).toBeTruthy();
    expect(screen.getByRole("button", { name: /Approve & triage/i })).toBeTruthy();
  });

  it("omits the banner without the needs:approval label", () => {
    renderCard({ ...baseIssue, labels: ["bug"] });
    expect(screen.queryByText(/approval required/i)).toBeNull();
  });
});

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

// The card's run pointer was the only thing the grid said about the run it
// launched (#1501): a card whose run died read exactly like a card that was
// dispatched and worked. The chip now carries the terminal status, and the
// title the cause — the grid has room for a word, the modal for the rest.
describe("IssueCard last-run outcome", () => {
  it("names the terminal status on the chip and the cause in its title", () => {
    renderCard({
      ...baseIssue,
      state: "blocked",
      last_run_id: "run-dead",
      last_run_status: "failed",
      last_run_error_code: "SANDBOX_DRIVER_UNAVAILABLE",
      last_run_error: "sandbox start: no runtime",
    });
    const chip = screen.getByRole("button", { name: /last run/i });
    expect(chip.textContent).toContain("failed");
    expect(chip.className).toContain("text-warning-fg");
    expect(chip.getAttribute("title")).toContain("SANDBOX_DRIVER_UNAVAILABLE");
    expect(chip.getAttribute("title")).toContain("sandbox start: no runtime");
  });

  it("keeps a successful run neutral", () => {
    renderCard({ ...baseIssue, state: "done", last_run_id: "run-ok", last_run_status: "finished" });
    const chip = screen.getByRole("button", { name: /last run/i });
    expect(chip.textContent).toContain("finished");
    expect(chip.className).toContain("text-info");
    expect(chip.className).not.toContain("text-warning-fg");
  });

  it("says nothing about an outcome the spine has not stamped yet", () => {
    renderCard({ ...baseIssue, state: "ready", last_run_id: "run-fresh" });
    const chip = screen.getByRole("button", { name: /last run/i });
    expect(chip.textContent).toBe("↪ last run");
  });

  // in_progress is the live signal on the cloud board: the chip opens the
  // RUNNING run, so a stale terminal status must not be painted over it.
  it("stays the live-run chip while the card is in progress", () => {
    renderCard({ ...baseIssue, state: "in_progress", last_run_id: "run-live", last_run_status: "failed" });
    const chip = screen.getByRole("button", { name: /live run/i });
    expect(chip.textContent).toBe("▶ live run");
    expect(chip.className).toContain("text-info");
  });
});
