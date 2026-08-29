// @vitest-environment jsdom
//
// Coming back to a conversation, you want what it is ABOUT — for one that
// drafted a workflow, the editor with that draft. Reported as: the link took
// the operator to the board they happened to be on when they opened the tab,
// which is where the conversation was BORN, not where its work lives.
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

vi.mock("wouter", () => ({
  Link: ({ href, children }: { href: string; children: React.ReactNode }) => (
    <a href={href}>{children}</a>
  ),
}));

import { WorkplaceLink } from "./ConversationStrip";

const bornOnBoard = {
  id: "c1",
  botId: "copilot",
  origin: "view/board",
  originLabel: "Board",
};

afterEach(cleanup);

function href() {
  return screen.getByRole("link").getAttribute("href");
}

describe("where a conversation takes you back to", () => {
  it("takes you to the workflow it drafted, not to where it started", () => {
    render(
      <WorkplaceLink
        conversation={bornOnBoard}
        runId="run-1"
        hasDraft
        currentPath="/runs"
        currentSearch=""
      />,
    );
    expect(href()).toBe("/editor?draft=run-1");
  });

  it("falls back to where it started when it drafted nothing", () => {
    render(
      <WorkplaceLink
        conversation={bornOnBoard}
        runId="run-1"
        hasDraft={false}
        currentPath="/runs"
        currentSearch=""
      />,
    );
    expect(href()).toBe("/board");
  });

  // A link to where you stand is noise.
  it("offers nothing when you are already looking at that draft", () => {
    render(
      <WorkplaceLink
        conversation={bornOnBoard}
        runId="run-1"
        hasDraft
        currentPath="/editor"
        currentSearch="?draft=run-1"
      />,
    );
    expect(screen.queryByRole("link")).toBeNull();
  });

  it("still offers the draft when the editor shows a DIFFERENT one", () => {
    render(
      <WorkplaceLink
        conversation={bornOnBoard}
        runId="run-1"
        hasDraft
        currentPath="/editor"
        currentSearch="?draft=other-run"
      />,
    );
    expect(href()).toBe("/editor?draft=run-1");
  });

  it("offers nothing when you are already on the origin page", () => {
    render(
      <WorkplaceLink
        conversation={bornOnBoard}
        runId={null}
        hasDraft={false}
        currentPath="/board"
        currentSearch=""
      />,
    );
    expect(screen.queryByRole("link")).toBeNull();
  });

  it("offers nothing when there is nowhere to go", () => {
    render(
      <WorkplaceLink
        conversation={{ id: "c2", botId: "copilot" }}
        runId={null}
        hasDraft={false}
        currentPath="/runs"
        currentSearch=""
      />,
    );
    expect(screen.queryByRole("link")).toBeNull();
  });
});
