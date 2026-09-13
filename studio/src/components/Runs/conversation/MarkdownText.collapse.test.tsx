// @vitest-environment jsdom
//
// In a conversation, the prose is the message and a pasted `.bot` is the
// evidence behind it. Unfolded, a forty-line workflow buries the one sentence
// that explains it — the operator scrolls past the answer to reach the answer.
//
// So `collapsibleCode` folds LONG fenced blocks and leaves everything else
// exactly as it was: this option must not change how the rest of the studio
// renders markdown.
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import MarkdownText from "./MarkdownText";

const LONG = ["one", "two", "three", "four", "five", "six"].join("\n");

afterEach(cleanup);

describe("folding code in a conversation", () => {
  it("keeps the prose visible and folds the block", () => {
    render(
      <MarkdownText
        value={`Voici le bot minimal :\n\n\`\`\`\n${LONG}\n\`\`\``}
        collapsibleCode
      />,
    );
    expect(screen.getByText(/Voici le bot minimal/)).toBeTruthy();
    const details = document.querySelector("details");
    expect(details).toBeTruthy();
    // Closed by default — the operator opens it when they want it.
    expect(details?.open).toBe(false);
  });

  it("says how much is hidden, so opening it is an informed click", () => {
    render(<MarkdownText value={`\`\`\`\n${LONG}\n\`\`\``} collapsibleCode />);
    expect(screen.getByText(/6 lines/)).toBeTruthy();
  });

  it("still contains the code, so nothing is lost by folding", () => {
    render(<MarkdownText value={`\`\`\`\n${LONG}\n\`\`\``} collapsibleCode />);
    expect(document.querySelector("details pre")?.textContent).toContain("three");
  });

  it("leaves a short snippet open — a click to read three words is worse", () => {
    render(<MarkdownText value={"```\na\nb\n```"} collapsibleCode />);
    expect(document.querySelector("details")).toBeNull();
    expect(document.querySelector("pre")).toBeTruthy();
  });

  it("never folds inline code", () => {
    render(<MarkdownText value={"use `worktree: auto` here"} collapsibleCode />);
    expect(document.querySelector("details")).toBeNull();
  });

  it("changes nothing without the option — every other surface is untouched", () => {
    render(<MarkdownText value={`\`\`\`\n${LONG}\n\`\`\``} />);
    expect(document.querySelector("details")).toBeNull();
    expect(document.querySelector("pre")?.textContent).toContain("three");
  });
});
