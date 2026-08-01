// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

import { TraceBlock } from "./LLMTrace";

afterEach(cleanup);

const LONG_PROMPT = Array.from({ length: 50 }, (_, i) => `rule ${i}`).join("\n");

// The run console's trace blocks and the pipeline card drawer share
// ExpandableValue, so a long prompt behaves identically on both surfaces.
describe("TraceBlock", () => {
  it("collapses a long prompt to a preview and expands it in place", () => {
    const { container } = render(
      <TraceBlock title="system prompt" body={LONG_PROMPT} />,
    );
    const pre = container.querySelector("pre");
    // Nothing is dropped — the whole prompt is in the DOM, height-capped.
    expect(pre?.textContent).toBe(LONG_PROMPT);
    expect(pre?.style.maxHeight).toBe("15rem");

    fireEvent.click(screen.getByRole("button", { name: "Show all 50 lines" }));
    expect(container.querySelector("pre")?.style.maxHeight).toBe("");
  });

  it("leaves a short body uncapped, with no expand toggle", () => {
    const { container } = render(<TraceBlock title="response" body="ok" />);
    expect(container.querySelector("pre")?.style.maxHeight).toBe("");
    expect(screen.queryByRole("button", { name: /show all/i })).toBeNull();
  });

  it("copy lives on the value (so it follows the raw/pretty toggle), not the summary", () => {
    render(<TraceBlock title="response" body='{"verdict":"approve"}' />);
    expect(screen.getByRole("button", { name: "Copy response" })).toBeTruthy();
    // A JSON response is pretty-printed by default and can be flipped back.
    expect(screen.getByRole("button", { name: "Show raw response" })).toBeTruthy();
    expect(screen.getByText(/"verdict": "approve"/)).toBeTruthy();
  });

  it("keeps the section fold — the body is still a <details> the user can close", () => {
    const { container } = render(
      <TraceBlock title="user message" body="hello" defaultOpen={false} />,
    );
    const details = container.querySelector("details");
    expect(details?.open).toBe(false);
    expect(screen.getByText("user message")).toBeTruthy();
  });
});
