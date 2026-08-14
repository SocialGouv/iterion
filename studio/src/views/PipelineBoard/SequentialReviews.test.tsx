// @vitest-environment jsdom

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { renderToStaticMarkup } from "react-dom/server";
import { afterEach, describe, expect, it, vi } from "vitest";

import type {
  PipelineBoardCard,
  PipelineBoardPendingReview,
} from "@/api/pipelineBoards";

afterEach(cleanup);

// The review panel fetches its change range, so this tree needs a query
// client. retry:false settles the error path on the first rejection.
function withClient(ui: React.ReactElement) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={qc}>{ui}</QueryClientProvider>;
}

vi.mock("wouter", () => ({
  Link: ({
    href,
    children,
    ...props
  }: React.AnchorHTMLAttributes<HTMLAnchorElement> & { href: string }) => (
    <a href={href} {...props}>
      {children}
    </a>
  ),
}));

// The real HumanPromptForm owns the draft + answer POST. This small stateful
// DOM stand-in lets the queue test prove that React keeps the exact active
// form mounted (and therefore preserves its in-progress draft) across polls.
vi.mock("@/components/Runs/conversation/HumanPromptForm", () => ({
  default: (props: {
    runId: string;
    nodeId: string;
    questions: Record<string, unknown>;
    sourceOverride?: string | null;
    onResumed?: () => void;
  }) => (
    <div
      data-testid="human-prompt"
      data-run-id={props.runId}
      data-node-id={props.nodeId}
      data-question={String(props.questions.q ?? "")}
      data-source-null={props.sourceOverride === null ? "yes" : "no"}
    >
      <input aria-label="Review draft" defaultValue="" />
      <button type="button" onClick={props.onResumed}>
        Resolve
      </button>
    </div>
  ),
}));

// Distinct from the form mock so the stepper test can count leftover
// panels. data-scope-run-id (not data-run-id) keeps the static-markup
// assertions on the form from matching this stand-in.
vi.mock("./ReviewScopePanel", () => ({
  ReviewScopePanel: (props: { runId: string; pauseKey?: string }) => (
    <div
      data-testid="review-scope"
      data-scope-run-id={props.runId}
      data-pause-key={props.pauseKey ?? ""}
    />
  ),
}));

import {
  clampReviewIndex,
  pendingReviewVersionKey,
  sortPendingReviewsChronologically,
} from "./reviewQueue";
import { SequentialReviews } from "./SequentialReviews";

function cardWithReviews(count: number): PipelineBoardCard {
  const pending = Array.from({ length: count }, (_, i) => ({
    run_id: `run-${i}`,
    node_id: `review_${i}`,
    interaction_id: `interaction-${i}`,
    updated_at: `2026-07-14T10:0${i}:00Z`,
    depth: i,
    questions: { q: `Q${i}?` },
  }));
  return {
    id: "run:root",
    kind: "run",
    column_id: "in_progress",
    title: "Root",
    run_id: "run-root",
    executed_nodes: 0,
    total_nodes: 0,
    tree_executed_nodes: 0,
    tree_total_nodes: 0,
    created_at: "2026-07-14T09:00:00Z",
    updated_at: "2026-07-14T10:00:00Z",
    pending_reviews: pending,
  };
}

describe("clampReviewIndex", () => {
  it("keeps the index inside the pending set (Prev at 0 / Next at last are no-ops)", () => {
    expect(clampReviewIndex(-1, 3)).toBe(0);
    expect(clampReviewIndex(0, 3)).toBe(0);
    expect(clampReviewIndex(1, 3)).toBe(1);
    expect(clampReviewIndex(5, 3)).toBe(2);
    expect(clampReviewIndex(0, 0)).toBe(0);
  });
});

describe("review queue helpers", () => {
  it("orders pending turns oldest first and versions a reused interaction by update", () => {
    const a = cardWithReviews(1).pending_reviews?.[0] as PipelineBoardPendingReview;
    const a2 = { ...a, updated_at: "2026-07-14T10:03:00Z" };
    const b = {
      ...a,
      run_id: "run-b",
      interaction_id: "interaction-b",
      updated_at: "2026-07-14T10:01:00Z",
    };
    expect(sortPendingReviewsChronologically([a2, b]).map((r) => r.run_id)).toEqual([
      "run-b",
      "run-0",
    ]);
    expect(pendingReviewVersionKey(a2)).not.toBe(pendingReviewVersionKey(a));
  });
});

describe("SequentialReviews", () => {
  it("mounts the first review one at a time, resuming with no source override", () => {
    const html = renderToStaticMarkup(
      withClient(
      <SequentialReviews card={cardWithReviews(2)} onResolved={() => {}} />,
      ),
    );

    expect(html).toContain("Review 1 of 2");
    // Prev / Next stepper is present for a multi-review set.
    expect(html).toContain('aria-label="Previous review"');
    expect(html).toContain('aria-label="Next review"');
    // Only the current review's form is mounted.
    expect(html).toContain('data-run-id="run-0"');
    expect(html).toContain('data-node-id="review_0"');
    expect(html).not.toContain('data-run-id="run-1"');
    // Board caller sends NO workflow source (server uses the run's FilePath).
    expect(html).toContain('data-source-null="yes"');
  });

  it("wires a successful answer to onResolved (the board refetch)", () => {
    const onResolved = vi.fn();
    render(
      withClient(
        <SequentialReviews card={cardWithReviews(2)} onResolved={onResolved} />,
      ),
    );
    fireEvent.click(screen.getByRole("button", { name: "Resolve" }));
    expect(onResolved).toHaveBeenCalledTimes(1);
  });

  it("keeps the active form and draft mounted while a newer AI turn joins the back", () => {
    const pending = cardWithReviews(3).pending_reviews;
    const a = pending?.[0];
    const b = pending?.[1];
    const c = pending?.[2];
    if (!a || !b || !c) throw new Error("test requires three pending reviews");
    const a2: PipelineBoardPendingReview = {
      ...a,
      updated_at: "2026-07-14T10:03:00Z",
      questions: { q: "A2?" },
    };
    const view = render(
      withClient(
        <SequentialReviews
          card={{ ...cardWithReviews(3), pending_reviews: [c, a, b] }}
          onResolved={() => {}}
        />,
      ),
    );

    // FIFO sorting starts on A; resolving it immediately pins the next turn B
    // even before the board refetch has returned.
    expect(screen.getByTestId("human-prompt").getAttribute("data-run-id")).toBe(a.run_id);
    fireEvent.click(screen.getByRole("button", { name: "Resolve" }));
    expect(screen.getByTestId("human-prompt").getAttribute("data-run-id")).toBe(b.run_id);
    fireEvent.change(screen.getByRole("textbox", { name: "Review draft" }), {
      target: { value: "draft for B" },
    });

    // A's next turn has the same run/node/interaction identity but a newer
    // update. It joins the back; polling must not replace B with C or A2.
    view.rerender(withClient(
      <SequentialReviews
        card={{ ...cardWithReviews(3), pending_reviews: [b, c, a2] }}
        onResolved={() => {}}
      />,
    ));
    expect(screen.getByTestId("human-prompt").getAttribute("data-run-id")).toBe(b.run_id);
    expect(
      (screen.getByRole("textbox", { name: "Review draft" }) as HTMLInputElement).value,
    ).toBe("draft for B");
    expect(screen.getByTestId("human-prompt").getAttribute("data-question")).not.toBe("A2?");

    // Once B is resolved/removed, the next oldest turn C gets the screen.
    // A2 appears only when the operator explicitly advances to its turn.
    view.rerender(withClient(
      <SequentialReviews
        card={{ ...cardWithReviews(3), pending_reviews: [c, a2] }}
        onResolved={() => {}}
      />,
    ));
    expect(screen.getByTestId("human-prompt").getAttribute("data-run-id")).toBe(c.run_id);
    fireEvent.click(screen.getByRole("button", { name: "Next review" }));
    expect(screen.getByTestId("human-prompt").getAttribute("data-question")).toBe("A2?");
  });

  it("renders nothing when there are no pending reviews", () => {
    const card = { ...cardWithReviews(0), pending_reviews: [] };
    const html = renderToStaticMarkup(
      withClient(
      <SequentialReviews card={card} onResolved={() => {}} />,
      ),
    );
    expect(html).toBe("");
  });

  it("replaces the review-scope panel when stepping to another turn", () => {
    // Two siblings used to share reviewKey. React's remaining-children
    // map keeps only the last child per key, so Next unmounted the form
    // and leaked every previous ReviewScopePanel.
    const card = cardWithReviews(3);
    const first = card.pending_reviews?.[0];
    const second = card.pending_reviews?.[1];
    if (!first || !second) throw new Error("test requires three pending reviews");
    render(
      withClient(
        <SequentialReviews card={card} onResolved={() => {}} />,
      ),
    );
    expect(screen.getAllByTestId("review-scope")).toHaveLength(1);
    expect(screen.getByTestId("review-scope").getAttribute("data-scope-run-id")).toBe(
      "run-0",
    );
    expect(screen.getByTestId("review-scope").getAttribute("data-pause-key")).toBe(
      pendingReviewVersionKey(first),
    );

    fireEvent.click(screen.getByRole("button", { name: "Next review" }));
    expect(screen.getAllByTestId("review-scope")).toHaveLength(1);
    expect(screen.getByTestId("review-scope").getAttribute("data-scope-run-id")).toBe(
      "run-1",
    );
    // pauseKey cache-keys the scope query. The Fragment remounts the
    // panel, but a remount does not evict react-query's entry — without
    // a per-turn pauseKey a second gate on the same run_id reuses N-1.
    expect(screen.getByTestId("review-scope").getAttribute("data-pause-key")).toBe(
      pendingReviewVersionKey(second),
    );

    fireEvent.click(screen.getByRole("button", { name: "Next review" }));
    fireEvent.click(screen.getByRole("button", { name: "Previous review" }));
    expect(screen.getAllByTestId("review-scope")).toHaveLength(1);
    expect(screen.getByTestId("review-scope").getAttribute("data-scope-run-id")).toBe(
      "run-1",
    );
    expect(screen.getByTestId("review-scope").getAttribute("data-pause-key")).toBe(
      pendingReviewVersionKey(second),
    );
    expect(screen.getByTestId("human-prompt").getAttribute("data-run-id")).toBe("run-1");
  });
});
