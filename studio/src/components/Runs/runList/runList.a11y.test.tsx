// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { RunSummary } from "@/api/runs";
import { expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";

import { RunListCard } from "./RunListCard";
import { RunListRow } from "./RunListRow";

afterEach(cleanup);

// The runs list is the studio's densest repetition: one Resume control per
// resumable run, all of them reading "Resume". The visible word stays, the
// accessible name carries the run (#2591's surface triage).
// NO custom name on purpose: friendlyLabel then falls back to
// workflow_name, which every run of one workflow shares — the hostile case,
// and the ordinary one on this surface. The short id is what separates them,
// and the last cell shows it.
function run(id: string, name?: string): RunSummary {
  return {
    id,
    ...(name ? { name } : {}),
    workflow_name: "deploy",
    status: "failed_resumable",
    created_at: "2026-10-01T10:00:00Z",
    updated_at: "2026-10-01T10:05:00Z",
    active: false,
  } as RunSummary;
}

const noop = () => {};

function mountRows() {
  return render(
    <table>
      <tbody>
        <RunListRow
          run={run("0199aaaa-1111-7000-8000-000000000001")}
          selected={false}
          resuming={false}
          onOpen={noop}
          onFilterBot={noop}
          onToggleSelect={noop}
          onResume={noop}
        />
        <RunListRow
          run={run("0199bbbb-2222-7000-8000-000000000002")}
          selected={false}
          resuming={false}
          onOpen={noop}
          onFilterBot={noop}
          onToggleSelect={noop}
          onResume={noop}
        />
      </tbody>
    </table>,
  );
}

describe("runs list naming", () => {
  it("separates two unnamed runs of the SAME workflow, row by row", () => {
    mountRows();
    // Both rows read "deploy": the name has to carry the short id, or the
    // list offers two controls a screen reader cannot tell apart.
    const resume = screen.getAllByRole("button", { name: /^Resume deploy / });
    expect(resume).toHaveLength(2);
    expect(new Set(resume.map((b) => b.getAttribute("aria-label"))).size).toBe(2);
    expect(
      screen.getByRole("button", { name: "Resume deploy 0199aaaa-1111-" }),
    ).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Resume deploy 0199bbbb-2222-" }),
    ).toBeTruthy();
    // The visible word is untouched, so SC 2.5.3 still holds.
    for (const b of resume) expect(b.textContent).toContain("Resume");
  });

  it("names the card's Resume the same way — touch has the same list", () => {
    const onResume = vi.fn();
    render(
      <RunListCard
        run={run("0199cccc-3333-7000-8000-000000000003")}
        resuming={false}
        onOpen={noop}
        onFilterBot={noop}
        onResume={onResume}
      />,
    );
    const button = screen.getByRole("button", { name: "Resume deploy 0199cccc-3333-" });
    expect(button.textContent).toContain("Resume");
  });

  it("qualifies each row's select and drag controls the same way", () => {
    mountRows();
    for (const name of [
      "Select run deploy 0199aaaa-1111-",
      "Select run deploy 0199bbbb-2222-",
    ]) {
      expect(screen.getAllByRole("checkbox", { name })).toHaveLength(1);
    }
    for (const name of [
      "Drag deploy 0199aaaa-1111- onto the assistant",
      "Drag deploy 0199bbbb-2222- onto the assistant",
    ]) {
      expect(screen.getAllByRole("img", { name })).toHaveLength(1);
    }
  });

  it("leaves no two ROW-SPECIFIC controls sharing an accessible name", () => {
    mountRows();
    // Scoped to the controls that act on one row. The bot avatar is
    // deliberately out: every row's avatar runs the same filter, and two
    // controls with one name doing one thing is not a defect.
    const rowSpecific = [
      ...screen.getAllByRole("button", { name: /^Resume / }),
      ...screen.getAllByRole("checkbox"),
      ...screen.getAllByRole("img", { name: /onto the assistant$/ }),
    ];
    expect(rowSpecific.length).toBe(6);
    const names = rowSpecific.map((el) => el.getAttribute("aria-label"));
    expect(names.every((n) => n && n.length > 0)).toBe(true);
    expect(new Set(names).size).toBe(names.length);
  });
});

describe("runs list axe", () => {
  it("Runs · list rows — axe smoke, incompletes included", async () => {
    const { container } = mountRows();
    await expectNoViolationsStrict(container, "RunListRow");
  });

  // No axe entry for RunListCard yet, and the reason is greppable rather
  // than silent: the card spreads `clickableRowProps` (role="button" +
  // tabindex) on a container that holds the bot-avatar and Resume buttons,
  // so axe reports `nested-interactive` — a real defect, shared by the 14
  // call sites of that helper, tracked as #2766. Its entry lands with the
  // fix; the naming pin above holds in the meantime.
});
