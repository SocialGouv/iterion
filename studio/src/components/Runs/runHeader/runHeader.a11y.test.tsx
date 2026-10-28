// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import type { BackendUsage, FallbackUsage, RunHeader } from "@/api/runs";
import { expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";

import BackendsUsedRow from "./BackendsUsedRow";
import BotChip from "./BotChip";
import FallbacksUsedRow from "./FallbacksUsedRow";

afterEach(cleanup);

// The run header is a stack of named rows, each a set of chips. A name on a
// roleless div is prohibited ARIA (axe aria-prohibited-attr), so each row
// declares the group it names — and the persona glyph declares that it is a
// glyph with a text alternative (#2591's surface triage).
const BACKENDS: BackendUsage[] = [
  { backend: "claw", model: "openai/gpt-5.4-mini", node_count: 2 },
  { backend: "claude_code", model: "sonnet", node_count: 1 },
];

const FALLBACKS: FallbackUsage[] = [
  { node_id: "plan", served_by: "cheap-route", backend: "claw", model: "mini" },
];

function runHeader(): RunHeader {
  return {
    id: "run-1",
    workflow_name: "feature_dev",
    bundle_name: "feature-dev",
    bundle_display_name: "Fleur",
    status: "finished",
    created_at: "2026-10-01T10:00:00Z",
    updated_at: "2026-10-01T10:05:00Z",
    active_duration_ms: 0,
  } as RunHeader;
}

function mountRows() {
  return render(
    <main>
      <BackendsUsedRow backends={BACKENDS} />
      <FallbacksUsedRow fallbacks={FALLBACKS} />
      <BotChip run={runHeader()} fileBase="main.bot" onOpenFile={() => {}} />
    </main>,
  );
}

describe("run header naming", () => {
  it("exposes each row as the group it names", () => {
    mountRows();
    expect(screen.getByRole("group", { name: "Backends used" })).toBeTruthy();
    expect(
      screen.getByRole("group", { name: "Fallback routes used" }),
    ).toBeTruthy();
  });

  it("keeps the persona emoji out of the chip's name", () => {
    mountRows();
    // Decorative: the persona's name follows it, so a text alternative would
    // only prepend "persona bot" to every chip.
    expect(screen.queryByRole("img", { name: "persona bot" })).toBeNull();
    const chip = screen.getByRole("button", { name: /Fleur/ });
    expect(chip.getAttribute("aria-label")).toBeNull();
    expect(chip.textContent).not.toContain("persona bot");
  });
});

describe("run header axe", () => {
  it("Runs · header rows — axe smoke, incompletes included", async () => {
    const { container } = mountRows();
    await expectNoViolationsStrict(container, "RunHeader rows");
  });
});
