// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { BoardBinding } from "@/api/boardBinding";
import { BindingSummary } from "./ProjectBoardTab";

// The binding's two HEALTH readouts. They answer different questions and must
// not be rendered as one: a degradation is a column the board no longer
// carries, a sync conflict is a move a person made that the native board's
// terminal sink refuses (ADR-097 §7). Confusing them sends the operator
// hunting a column that is not missing.

function binding(over: Partial<BoardBinding> = {}): BoardBinding {
  return {
    tenant_id: "team-a",
    provider: "github",
    owner: "SocialGouv",
    owner_kind: "org",
    number: 203,
    connection_id: "conn-1",
    project_id: "PVT_p",
    status_field_id: "PVTSSF_status",
    status_mapping: [{ status: "Done", state: "done" }],
    sync_every_seconds: 120,
    created_at: "2026-09-06T10:00:00Z",
    updated_at: "2026-09-06T12:00:00Z",
    ...over,
  } as BoardBinding;
}

afterEach(cleanup);

describe("BindingSummary health readouts", () => {
  it("names a board move the terminal sink refused, and how to land it", () => {
    render(
      <BindingSummary
        binding={binding({
          sync_conflict_reason:
            "1 board move(s) refused: leaving a terminal column is a reopen — native:abc",
          sync_conflict_at: "2026-09-06T12:00:00Z",
        })}
      />,
    );
    expect(screen.getByText(/native:abc/)).toBeTruthy();
    // The refusal is not a broken column: the operator must not be sent
    // looking for a Status option that is perfectly present.
    expect(screen.queryByText(/no longer carries/)).toBeNull();
  });

  it("renders a lost column as a degradation, separately", () => {
    render(
      <BindingSummary
        binding={binding({
          degraded_reason: 'the Status field no longer carries "Done"',
        })}
      />,
    );
    expect(screen.getByText(/no longer carries/)).toBeTruthy();
  });

  it("shows neither on a healthy binding", () => {
    const { container } = render(<BindingSummary binding={binding()} />);
    expect(container.textContent).not.toMatch(/refused/i);
    expect(container.textContent).not.toMatch(/no longer carries/);
  });
});
