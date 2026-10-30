// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { RunSummary } from "@/api/runs";
import { expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";
import { onAssistantAttachRequest } from "@/lib/chatDock/attachToAssistant";
import type { TypedReference } from "@/lib/chatDock/routeReference";

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
      ...screen.getAllByRole("button", { name: /^Open / }),
      ...screen.getAllByRole("button", { name: /^Resume / }),
      ...screen.getAllByRole("checkbox"),
      ...screen.getAllByRole("img", { name: /onto the assistant$/ }),
    ];
    expect(rowSpecific.length).toBe(8);
    const names = rowSpecific.map((el) => el.getAttribute("aria-label"));
    expect(names.every((n) => n && n.length > 0)).toBe(true);
    expect(new Set(names).size).toBe(names.length);
  });
});

describe("runs list keyboard paths (#2766)", () => {
  it("the card opens from a real control, and the card itself is no button", () => {
    const onOpen = vi.fn();
    const { container } = render(
      <RunListCard
        run={run("0199cccc-3333-7000-8000-000000000003")}
        resuming={false}
        onOpen={onOpen}
        onFilterBot={noop}
        onResume={noop}
      />,
    );
    const card = container.firstElementChild as HTMLElement;
    // The card holds the bot-avatar and Resume buttons: a role="button"
    // here is a button containing buttons, and its Enter raced theirs.
    expect(card.getAttribute("role")).toBeNull();
    expect(card.hasAttribute("tabindex")).toBe(false);
    fireEvent.keyDown(card, { key: "Enter" });
    fireEvent.keyDown(card, { key: " " });
    expect(onOpen).not.toHaveBeenCalled();
    // The keyboard reaches the run through the title, a native control —
    // which is what makes the card's own click a pointer convenience and
    // not the only way in (RGAA 7.3).
    const title = screen.getByRole("button", { name: "Open deploy 0199cccc-3333-" });
    expect(title.tagName).toBe("BUTTON");
    expect(title.textContent).toBe("deploy");
    fireEvent.click(title);
    expect(onOpen).toHaveBeenCalledWith("0199cccc-3333-7000-8000-000000000003");
    // ... and the pointer convenience still works, once per click.
    fireEvent.click(card);
    expect(onOpen).toHaveBeenCalledTimes(2);
  });

  it("the table row opens from a real control too — one list, two layouts", () => {
    const onOpen = vi.fn();
    render(
      <table>
        <tbody>
          <RunListRow
            run={run("0199aaaa-1111-7000-8000-000000000001")}
            selected={false}
            resuming={false}
            onOpen={onOpen}
            onFilterBot={noop}
            onToggleSelect={noop}
            onResume={noop}
          />
        </tbody>
      </table>,
    );
    const row = screen.getByRole("row");
    expect(row.getAttribute("role")).toBeNull();
    expect(row.hasAttribute("tabindex")).toBe(false);
    const title = screen.getByRole("button", { name: "Open deploy 0199aaaa-1111-" });
    expect(title.tagName).toBe("BUTTON");
    fireEvent.click(title);
    expect(onOpen).toHaveBeenCalledWith("0199aaaa-1111-7000-8000-000000000001");
  });
});

describe("runs list drag-to-assistant (#2770)", () => {
  it("today the grip is a drag-only affordance, and no control offers the attach", () => {
    mountRows();
    // The current state, pinned honestly: each row's grip is draggable but
    // not focusable and claims no operation (role="img" is a text
    // alternative, not a widget), and no named control elsewhere on the row
    // performs the attach. SC 2.1.1 / 2.5.7 are exactly what #2770 still
    // owes — this test is meant to FAIL the day the placement lands, and to
    // be rewritten as the flip of the skipped one below.
    const grips = screen.getAllByRole("img", { name: /onto the assistant$/ });
    expect(grips).toHaveLength(2);
    for (const grip of grips) {
      expect(grip.getAttribute("draggable")).toBe("true");
      expect(grip.hasAttribute("tabindex")).toBe(false);
      expect(grip.tagName).not.toBe("BUTTON");
    }
    expect(
      screen.queryByRole("button", {
        name: /attach .+ (to|onto) the assistant/i,
      }),
    ).toBeNull();
  });

  // #2770 — enable with the placement Jo picks: the grip becoming a button,
  // or a named control elsewhere on the row. Written against the ticket's
  // "Attach <label> to the assistant" wording — adjust the name pattern to
  // the landed label, nothing else. Either placement must wire the control
  // to the shared attachReferenceToAssistant handler the drop path uses.
  it.skip("each row offers a keyboard-operable Attach control wired to the shared handler", () => {
    const received: TypedReference[] = [];
    const off = onAssistantAttachRequest((r) => received.push(r));
    try {
      mountRows();
      const controls = screen.getAllByRole("button", {
        name: /^Attach deploy .+ (to|onto) the assistant$/,
      });
      expect(controls).toHaveLength(2);
      const [first] = controls;
      if (!first) throw new Error("two rows, two attach controls");
      first.focus();
      expect(document.activeElement).toBe(first);
      // A native control turns Enter/Space into click — the keyboard path IS
      // the click handler (same idiom as the #2766 keyboard tests above).
      fireEvent.keyDown(first, { key: "Enter" });
      fireEvent.click(first);
      expect(received).toEqual([
        {
          kind: "run",
          ref: "run/0199aaaa-1111-7000-8000-000000000001",
          label: "deploy",
        },
      ]);
    } finally {
      off();
    }
  });
});

describe("runs list axe", () => {
  it("Runs · list rows — axe smoke, incompletes included", async () => {
    const { container } = mountRows();
    await expectNoViolationsStrict(container, "RunListRow");
  });

  // The card's entry, owed since #2766: it holds the bot-avatar and Resume
  // buttons, so while it spread `clickableRowProps` (role="button" +
  // tabindex) axe reported `nested-interactive` on it — the defect shared
  // by that helper's call sites. The row above and the card below are the
  // same list in two layouts, and both are judged.
  it("Runs · list card — axe smoke, incompletes included", async () => {
    const { container } = render(
      <main>
        <RunListCard
          run={run("0199cccc-3333-7000-8000-000000000003")}
          resuming={false}
          onOpen={noop}
          onFilterBot={noop}
          onResume={noop}
        />
      </main>,
    );
    await expectNoViolationsStrict(container, "RunListCard");
  });
});
