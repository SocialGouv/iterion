// @vitest-environment jsdom
import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render } from "@testing-library/react";

import MarkdownText from "./MarkdownText";

afterEach(cleanup);

// MarkdownText is the single markdown renderer shared by every
// conversation surface (run console agent output + operator replies,
// and the Nexie chat bubbles/cards). These tests lock the two
// capabilities that make the chat feel like Claude Code: GFM tables
// and syntax-highlighted code blocks. A regression in either (e.g. a
// dropped remark/rehype plugin) silently degrades every surface, so we
// assert on the rendered DOM, not the markdown string.
describe("MarkdownText", () => {
  it("renders a GFM table as a real <table>", () => {
    const md = ["| a | b |", "| - | - |", "| 1 | 2 |"].join("\n");
    const { container } = render(<MarkdownText value={md} />);
    const table = container.querySelector("table");
    expect(table).toBeTruthy();
    // header + one body row
    expect(container.querySelectorAll("th").length).toBe(2);
    expect(container.querySelectorAll("td").length).toBe(2);
  });

  it("syntax-highlights a fenced code block (hljs token classes)", () => {
    const md = ["```js", "const x = 1;", "```"].join("\n");
    const { container } = render(<MarkdownText value={md} />);
    const code = container.querySelector("code.hljs");
    expect(code).toBeTruthy();
    // rehype-highlight emits hljs-* token spans (e.g. the `const` keyword)
    expect(container.querySelector("[class*='hljs-']")).toBeTruthy();
  });

  it("styles inline code without treating it as a highlighted block", () => {
    const { container } = render(<MarkdownText value={"use `npm run build`"} />);
    const inline = container.querySelector("code");
    expect(inline).toBeTruthy();
    // inline code keeps the component-map class, not the hljs block class
    expect(inline?.classList.contains("hljs")).toBe(false);
    expect(inline?.className).toContain("font-mono");
  });

  it("renders lists and emphasis from plain prose", () => {
    const md = ["- **one**", "- two"].join("\n");
    const { container } = render(<MarkdownText value={md} />);
    expect(container.querySelectorAll("li").length).toBe(2);
    expect(container.querySelector("strong")?.textContent).toBe("one");
  });
});
