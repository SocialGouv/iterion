// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import {
  expectNoViolationsStrict,
  expectUniqueAccessibleNames,
} from "@/__tests__/a11y/axeHelpers";

import type { RunEvent } from "@/api/runs";

import { ToolCallList, ToolPayloadBlock } from "./ToolCalls";
import { groupToolCalls } from "./toolCallModel";

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
    // The tooltip is visible UI: it keeps the wording, and the row
    // qualification lives in the accessible name only.
    expect(copy.getAttribute("title")).toBe("copy");
    const toggle = screen.getByRole("button", {
      name: "show raw event of Bash seq 1",
    });
    expect(toggle.textContent).toBe("show raw");
  });

  it("leaves no two controls sharing an accessible name", () => {
    const { container } = mountTools();
    // The enumerated form: exactly these names, each read once, and no other
    // button on the surface — so a collision cannot hide behind a name this
    // test computed itself.
    expectUniqueAccessibleNames(container, "Runs · tool calls", {
      roles: ["button"],
      expected: [
        "copy raw input of Bash seq 1",
        "copy output of Bash seq 1",
        "show raw event of Bash seq 1",
        "copy raw input of Bash seq 2",
        "copy the error of Bash seq 2",
        "show raw event of Bash seq 2",
      ],
    });
  });
});

// The payload block's own two controls only exist when there is more to
// fetch (`hasMore`) or more to show (`overflow`) — neither of which a fixture
// gets for free: jsdom has no layout, so scrollHeight is stubbed, and the
// sidecar-blob props have to be supplied. Without this they are unexercised,
// and an unexercised control is where a name goes wrong unseen.
describe("tool payload block naming", () => {
  function mountBlock() {
    const view = render(
      <main>
        <ToolPayloadBlock
          label="output"
          source="Bash seq 7"
          value={"x".repeat(400)}
          runId="run-1"
          toolUseID="tu-1"
          kind="output"
          totalSize={200_000}
        />
      </main>,
    );
    const details = view.container.querySelector("details");
    if (!details) throw new Error("the payload block renders a disclosure");
    // jsdom queues <details>' own toggle event, so the open state is set the
    // way the browser would and the component's onToggle is driven directly
    // — the block's `overflow` measurement only runs while it is open.
    (details as HTMLDetailsElement).open = true;
    fireEvent(details, new Event("toggle"));
    return view;
  }

  it("names load more after what it renders, figure included (SC 2.5.3)", () => {
    mountBlock();
    const more = screen.getByRole("button", { name: /^load more \(\+/ });
    expect(more.textContent).toBe("load more (+64.0 KiB)");
    expect(more.getAttribute("aria-label")).toBe(
      "load more (+64.0 KiB) of output of Bash seq 7",
    );
  });

  it("names expand after what it renders, qualified by its row", () => {
    const pre = Object.getOwnPropertyDescriptor(
      HTMLElement.prototype,
      "scrollHeight",
    );
    // jsdom measures everything 0, so `overflow` can never become true and
    // the expand control never renders. Stubbed for this case only.
    Object.defineProperty(HTMLElement.prototype, "scrollHeight", {
      configurable: true,
      get: () => 9999,
    });
    try {
      mountBlock();
      const expand = screen.getByRole("button", { name: /^expand / });
      expect(expand.textContent).toBe("expand");
      expect(expand.getAttribute("aria-label")).toBe(
        "expand output of Bash seq 7",
      );
    } finally {
      if (pre) Object.defineProperty(HTMLElement.prototype, "scrollHeight", pre);
      else delete (HTMLElement.prototype as unknown as Record<string, unknown>)["scrollHeight"];
    }
  });
});

describe("tool calls axe", () => {
  it("Runs · detail Tools tab — axe smoke, incompletes included", async () => {
    const { container } = mountTools();
    await expectNoViolationsStrict(container, "ToolCallList");
  });
});

// A codemode program's card holds its nested calls: the same per-call
// controls repeat inside it, a batch adds its own group, and the program adds
// its nested-calls toggle — every name still has to carry its row.
function programEvent(seq: number, type: RunEvent["type"], data: Record<string, unknown>): RunEvent {
  return { seq, timestamp: "2026-01-01T00:00:00Z", type, run_id: "run-1", node_id: "n", data } as RunEvent;
}

const PROGRAM_EVENTS: RunEvent[] = [
  programEvent(1, "tool_started", { tool: "codemode", tool_use_id: "toolu_P", input: '{"source":"call(\\"glob\\", {})"}' }),
  programEvent(2, "tool_started", { tool: "glob", tool_use_id: "codemode_A_1", parent_tool_use_id: "toolu_P", input: '{"pattern":"*"}' }),
  programEvent(3, "tool_called", { tool: "glob", tool_use_id: "codemode_A_1", parent_tool_use_id: "toolu_P", output: "a.md" }),
  programEvent(4, "tool_error", {
    tool: "ask_user",
    tool_use_id: "codemode_A_2",
    parent_tool_use_id: "toolu_P",
    batch: { first: 2, size: 2, index: 0 },
    error: "pauses the run",
  }),
  programEvent(5, "tool_started", {
    tool: "glob",
    tool_use_id: "codemode_A_3",
    parent_tool_use_id: "toolu_P",
    batch: { first: 2, size: 2, index: 1 },
    input: '{"pattern":"b"}',
  }),
  programEvent(6, "tool_called", {
    tool: "glob",
    tool_use_id: "codemode_A_3",
    parent_tool_use_id: "toolu_P",
    batch: { first: 2, size: 2, index: 1 },
    output: "b.md",
  }),
  programEvent(7, "tool_error", {
    tool: "codemode",
    tool_use_id: "toolu_P",
    error: "codemode refused (script_error): Error: nope",
    codemode: {
      limits: { tool_calls: 4096, wall_ns: 30_000_000_000, memory_bytes: 268_435_456 },
      adjusted: [{ field: "tool_calls", requested: 99999, applied: 4096 }],
      refusal: "script_error",
      calls: 2,
      source_bytes: 18,
      confinement: { cpu_seconds: 1, memory_bytes: 1_342_177_280, all_threads: true, landlock_abi: 4 },
      effective_wall_ns: 2_000_000_000,
    },
  }),
];

function mountProgram() {
  return render(
    <main>
      <ToolCallList calls={groupToolCalls(PROGRAM_EVENTS)} runId="run-1" />
    </main>,
  );
}

describe("codemode program card", () => {
  it("shows the program, its verdict, its budget and the bound the clamp moved", () => {
    mountProgram();
    // The program block and the summary's first line both show the source.
    expect(screen.getAllByText("call(\"glob\", {})").length).toBe(2);
    expect(screen.getByText("script_error")).toBeTruthy();
    expect(screen.getByText(/calls 2 \/ 4096/)).toBeTruthy();
    const limits = screen.getByRole("table", { name: "limits requested for codemode seq 7" });
    expect(limits.textContent).toContain("99999 → 4096 (adjusted)");
    const enforced = screen.getByRole("table", { name: "limits enforced for codemode seq 7" });
    expect(enforced.textContent).toContain("cpu_seconds1s");
    expect(enforced.textContent).toContain("all_threadsyes");
    expect(screen.getByText(/wall cut to 2s by the node's deadline/)).toBeTruthy();
    expect(screen.getByText("call.parallel × 2")).toBeTruthy();
  });

  it("collapses and reopens the nested calls under one named toggle", () => {
    mountProgram();
    const toggle = screen.getByRole("button", { name: "nested calls (3) of codemode seq 7" });
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    expect(screen.getByRole("button", { name: "copy output of glob seq 3" })).toBeTruthy();
    fireEvent.click(toggle);
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByRole("button", { name: "copy output of glob seq 3" })).toBeNull();
  });

  it("leaves no two controls sharing an accessible name inside the nested list", () => {
    const { container } = mountProgram();
    expectUniqueAccessibleNames(container, "Runs · codemode program", {
      roles: ["button"],
      expected: [
        "copy program of codemode seq 7",
        "copy the error of codemode seq 7",
        "nested calls (3) of codemode seq 7",
        "copy raw input of glob seq 3",
        "copy output of glob seq 3",
        "show raw event of glob seq 3",
        "copy the error of ask_user seq 4",
        "show raw event of ask_user seq 4",
        "copy raw input of glob seq 6",
        "copy output of glob seq 6",
        "show raw event of glob seq 6",
        "show raw event of codemode seq 7",
      ],
    });
  });

  it("Runs · detail Tools tab, codemode program — axe smoke, incompletes included", async () => {
    const { container } = mountProgram();
    await expectNoViolationsStrict(container, "ToolCallList codemode");
  });
});
