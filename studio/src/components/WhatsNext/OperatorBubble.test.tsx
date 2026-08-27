// @vitest-environment jsdom
//
// The operator's turns are drawn ONE way. They used to differ — the opening
// message as a right-aligned tinted card with a status chip, every reply after
// it as a "You" bubble — so a conversation appeared to change format after its
// first line. Same person, same shape.
//
// And the machine-generated context lines are protocol, not speech: shown
// inside the operator's own bubble they read as something they typed, and
// repeat what the context chip above the composer already says.
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { OperatorBubble } from "./OperatorBubble";

afterEach(cleanup);

describe("the operator's bubble", () => {
  it("shows what they wrote", () => {
    render(<OperatorBubble text="dis moi bonjour" />);
    expect(screen.getByText("dis moi bonjour")).toBeTruthy();
  });

  it("hides the page-context line the studio prepended", () => {
    render(<OperatorBubble text={"[page context: view/editor]\nsalut"} />);
    expect(screen.getByText("salut")).toBeTruthy();
    expect(screen.queryByText(/page context/)).toBeNull();
  });

  it("hides an attached-reference line too", () => {
    render(
      <OperatorBubble
        text={"[page context: view/board]\n[attached: run/019f]\nregarde"}
      />,
    );
    expect(screen.queryByText(/attached:/)).toBeNull();
    expect(screen.getByText("regarde")).toBeTruthy();
  });

  it("names the outcome when there was no text to show", () => {
    render(<OperatorBubble text="" empty="approved" />);
    expect(screen.getByText("approved")).toBeTruthy();
  });

  // A settled message is just a message; a chip on every one is noise.
  it("carries no badge unless one is given", () => {
    const { container } = render(<OperatorBubble text="ok" />);
    expect(container.textContent).toBe("Youok");
  });

  it("shows a badge for a state worth naming", () => {
    render(<OperatorBubble text="ok" badge={<span>Queued</span>} />);
    expect(screen.getByText("Queued")).toBeTruthy();
  });

  it("does not collapse a message that is only context into a stray blank", () => {
    render(<OperatorBubble text="[page context: view/board]" empty="(empty reply)" />);
    expect(screen.getByText("(empty reply)")).toBeTruthy();
  });
});
