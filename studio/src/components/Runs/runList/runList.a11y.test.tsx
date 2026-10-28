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
function run(id: string, name: string): RunSummary {
  return {
    id,
    name,
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
          run={run("run-aaaa", "nightly deploy")}
          selected={false}
          resuming={false}
          onOpen={noop}
          onFilterBot={noop}
          onToggleSelect={noop}
          onResume={noop}
        />
        <RunListRow
          run={run("run-bbbb", "hotfix deploy")}
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
  it("names every row's Resume after the run it resumes", () => {
    mountRows();
    expect(
      screen.getByRole("button", { name: "Resume nightly deploy" }),
    ).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "Resume hotfix deploy" }),
    ).toBeTruthy();
    // The visible word is untouched, so SC 2.5.3 still holds.
    for (const b of screen.getAllByRole("button", { name: /^Resume / })) {
      expect(b.textContent).toContain("Resume");
    }
  });

  it("names the card's Resume the same way — touch has the same list", () => {
    const onResume = vi.fn();
    render(
      <RunListCard
        run={run("run-cccc", "card deploy")}
        resuming={false}
        onOpen={noop}
        onFilterBot={noop}
        onResume={onResume}
      />,
    );
    const button = screen.getByRole("button", { name: "Resume card deploy" });
    expect(button.textContent).toContain("Resume");
  });

  it("keeps each row's select and drag controls qualified by the run", () => {
    mountRows();
    expect(
      screen.getByRole("checkbox", { name: "Select run nightly deploy" }),
    ).toBeTruthy();
    expect(
      screen.getByRole("checkbox", { name: "Select run hotfix deploy" }),
    ).toBeTruthy();
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
