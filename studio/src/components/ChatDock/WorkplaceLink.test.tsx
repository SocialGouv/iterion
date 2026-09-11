// @vitest-environment jsdom
// A produced workflow is a secondary destination. The immutable page anchor
// lives in ContextChip and must never be replaced by this link.
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

vi.mock("wouter", () => ({
  Link: ({ href, children }: { href: string; children: React.ReactNode }) => (
    <a href={href}>{children}</a>
  ),
}));

import { WorkplaceLink } from "./ConversationStrip";

afterEach(cleanup);

function renderLink(props: Partial<Parameters<typeof WorkplaceLink>[0]> = {}) {
  return render(
    <WorkplaceLink
      runId="run-1"
      hasDraft
      currentPath="/runs"
      currentSearch=""
      {...props}
    />,
  );
}

describe("the produced-workflow link", () => {
  it("links to the workflow without owning the conversation anchor", () => {
    renderLink();
    expect(screen.getByRole("link").getAttribute("href")).toBe(
      "/editor?draft=run-1",
    );
  });

  it("offers nothing when the conversation produced no draft", () => {
    renderLink({ hasDraft: false });
    expect(screen.queryByRole("link")).toBeNull();
  });

  it("offers nothing on the same draft", () => {
    renderLink({ currentPath: "/editor", currentSearch: "?draft=run-1" });
    expect(screen.queryByRole("link")).toBeNull();
  });

  it("distinguishes two editor queries", () => {
    renderLink({ currentPath: "/editor", currentSearch: "?draft=other" });
    expect(screen.getByRole("link").getAttribute("href")).toBe(
      "/editor?draft=run-1",
    );
  });
});
