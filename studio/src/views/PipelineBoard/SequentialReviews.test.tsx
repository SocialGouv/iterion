import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";

import type { PipelineBoardCard } from "@/api/pipelineBoards";

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

// The real HumanPromptForm owns the answer POST + resume. The mock forwards
// the identity props and, crucially, invokes `onResumed` during render so the
// static-render assertions can observe SequentialReviews wiring it to
// `onResolved` (the board refetch) without a DOM click.
vi.mock("@/components/Runs/conversation/HumanPromptForm", () => ({
  default: (props: {
    runId: string;
    nodeId: string;
    sourceOverride?: string | null;
    onResumed?: () => void;
  }) => {
    props.onResumed?.();
    return (
      <div
        data-testid="human-prompt"
        data-run-id={props.runId}
        data-node-id={props.nodeId}
        data-source-null={props.sourceOverride === null ? "yes" : "no"}
      />
    );
  },
}));

import { SequentialReviews, clampReviewIndex } from "./SequentialReviews";

function cardWithReviews(count: number): PipelineBoardCard {
  const pending = Array.from({ length: count }, (_, i) => ({
    run_id: `run-${i}`,
    node_id: `review_${i}`,
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

describe("SequentialReviews", () => {
  it("mounts the first review one at a time, resuming with no source override", () => {
    const html = renderToStaticMarkup(
      <SequentialReviews card={cardWithReviews(2)} onResolved={() => {}} />,
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
    renderToStaticMarkup(
      <SequentialReviews card={cardWithReviews(2)} onResolved={onResolved} />,
    );
    // The mock invokes onResumed for the single mounted review; the component
    // must route it to onResolved.
    expect(onResolved).toHaveBeenCalledTimes(1);
  });

  it("renders nothing when there are no pending reviews", () => {
    const card = { ...cardWithReviews(0), pending_reviews: [] };
    const html = renderToStaticMarkup(
      <SequentialReviews card={card} onResolved={() => {}} />,
    );
    expect(html).toBe("");
  });
});
