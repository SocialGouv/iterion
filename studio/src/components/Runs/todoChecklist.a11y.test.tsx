// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";

import { TodoItems } from "./todoChecklist";

afterEach(cleanup);

// The agent task list the Logs panel, the Session board and PlansPanel all
// render. Its status glyph carries a text alternative, so role="img" makes
// that alternative the WHOLE announcement — the wire value ("in_progress")
// is not something to read out (#2591's surface triage).
const TODOS = [
  {
    content: "write the net",
    status: "in_progress" as const,
    activeForm: "writing the net",
  },
  { content: "land it", status: "pending" as const },
  { content: "read the ticket", status: "completed" as const },
];

describe("todo checklist naming", () => {
  it("announces each state as a word, not as its wire value", () => {
    render(
      <main>
        <TodoItems todos={TODOS} />
      </main>,
    );
    expect(screen.getByRole("img", { name: "in progress" })).toBeTruthy();
    expect(screen.getByRole("img", { name: "pending" })).toBeTruthy();
    expect(screen.getByRole("img", { name: "completed" })).toBeTruthy();
    // The in-progress row shows its natural-language form, which the glyph
    // does not repeat.
    expect(screen.getByText("writing the net")).toBeTruthy();
  });
});

describe("todo checklist axe", () => {
  it("Runs · todo checklist — axe smoke, incompletes included", async () => {
    const { container } = render(
      <main>
        <TodoItems todos={TODOS} />
      </main>,
    );
    await expectNoViolationsStrict(container, "TodoItems");
  });
});
