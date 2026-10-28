// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { expectNoViolationsStrict } from "@/__tests__/a11y/axeHelpers";

import { ToolCallList, ToolPayloadBlock } from "./ToolCalls";

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
    // Every name is matched by the REAL accessible-name algorithm (the
    // `name` option of getAllByRole), one button each, and the surface holds
    // no other button — so a collision cannot hide behind a name this test
    // computed itself.
    const expected = [
      "copy raw input of Bash seq 1",
      "copy output of Bash seq 1",
      "show raw event of Bash seq 1",
      "copy raw input of Bash seq 2",
      "copy the error of Bash seq 2",
      "show raw event of Bash seq 2",
    ];
    for (const name of expected) {
      expect(screen.getAllByRole("button", { name })).toHaveLength(1);
    }
    expect(screen.getAllByRole("button")).toHaveLength(expected.length);
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
