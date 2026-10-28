// @vitest-environment jsdom
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";

import { ToolCallList } from "./ToolCalls";

afterEach(cleanup);

// A node's Tools tab repeats the same three controls per call — copy the
// input, copy the output, copy the error — and the same tool runs several
// times per node, so the name has to carry the row: the tool plus the `seq`
// the card already shows (#2591's surface triage). Visible text untouched.
const CALLS = [
  {
    seq: 1,
    toolName: "Bash",
    isError: false,
    rawData: { input: { command: "ls" } },
    input: '{"command":"ls"}',
    output: "a\nb\n",
  },
  {
    seq: 2,
    toolName: "Bash",
    isError: true,
    rawData: { input: { command: "boom" } },
    input: '{"command":"boom"}',
    output: "",
    errorMsg: "exit status 1",
  },
];

function mountTools() {
  return render(
    <main>
      <ToolCallList calls={CALLS} runId="run-1" />
    </main>,
  );
}

describe("tool calls naming", () => {
  it("names each payload copy after the call it belongs to", () => {
    mountTools();
    expect(
      screen.getByRole("button", { name: "copy raw input of Bash seq 1" }),
    ).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "copy output of Bash seq 1" }),
    ).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "copy raw input of Bash seq 2" }),
    ).toBeTruthy();
  });

  it("names the error copy and the raw toggle after the same row", () => {
    mountTools();
    expect(
      screen.getByRole("button", { name: "copy the error of Bash seq 2" }),
    ).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "show raw event of Bash seq 1" }),
    ).toBeTruthy();
    expect(
      screen.getByRole("button", { name: "show raw event of Bash seq 2" }),
    ).toBeTruthy();
  });

  it("keeps the visible word first, so the name contains the label (SC 2.5.3)", () => {
    mountTools();
    const copy = screen.getByRole("button", {
      name: "copy raw input of Bash seq 1",
    });
    expect(copy.textContent).toBe("copy");
    const toggle = screen.getByRole("button", {
      name: "show raw event of Bash seq 1",
    });
    expect(toggle.textContent).toBe("show raw");
  });

  it("leaves no two controls sharing an accessible name", () => {
    mountTools();
    const names = screen
      .getAllByRole("button")
      .map((b) => b.getAttribute("aria-label") ?? b.textContent ?? "");
    expect(names.length).toBeGreaterThan(4);
    expect(new Set(names).size).toBe(names.length);
  });
});

describe("tool calls axe", () => {
  it("Runs · detail Tools tab — axe smoke, incompletes included", async () => {
    const { container } = mountTools();
    await expectNoViolationsStrict(container, "ToolCallList");
  });
});
