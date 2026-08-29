// @vitest-environment jsdom

import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import {
  AssistantPageContextProvider,
  mergePageContextContributions,
  pageContextSnapshot,
  useAssistantPageContext,
  useRegisteredAssistantPageContext,
} from "./pageContext";
import { referenceForRoute } from "./routeReference";

afterEach(cleanup);

describe("page context contributions", () => {
  it("merges a page identity with a nested panel's visible state", () => {
    expect(
      mergePageContextContributions([
        {
          title: "review-pr",
          entity: { type: "bot", id: "review-pr" },
          state: { dirty: true },
        },
        { section: "agent-inspector", state: { selectedNode: "reviewer" } },
      ]),
    ).toEqual({
      title: "review-pr",
      section: "agent-inspector",
      entity: { type: "bot", id: "review-pr" },
      state: { dirty: true, selectedNode: "reviewer" },
    });
  });

  it("builds an automatic route floor and lets the page enrich it", () => {
    expect(
      pageContextSnapshot("/runs/019f", referenceForRoute("/runs/019f"), {
        section: "events",
        state: { followLive: false },
      }),
    ).toEqual({
      route: "/runs/019f",
      title: "Run 019f",
      section: "events",
      entity: { type: "run", id: "019f", label: "Run 019f" },
      state: { followLive: false },
    });
  });

  it("ignores mounted-but-hidden views", () => {
    function Publisher({ enabled }: { enabled: boolean }) {
      useAssistantPageContext({ title: "Hidden editor" }, enabled);
      return null;
    }
    function Reader() {
      const context = useRegisteredAssistantPageContext();
      return <span data-testid="context">{context?.title ?? "none"}</span>;
    }

    const view = render(
      <AssistantPageContextProvider>
        <Publisher enabled={false} />
        <Reader />
      </AssistantPageContextProvider>,
    );
    expect(screen.getByTestId("context").textContent).toBe("none");

    view.rerender(
      <AssistantPageContextProvider>
        <Publisher enabled />
        <Reader />
      </AssistantPageContextProvider>,
    );
    expect(screen.getByTestId("context").textContent).toBe("Hidden editor");
  });
});
