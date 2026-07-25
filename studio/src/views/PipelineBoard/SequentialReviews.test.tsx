// @vitest-environment jsdom

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { renderToStaticMarkup } from "react-dom/server";
import { afterEach, describe, expect, it, vi } from "vitest";

import type {
  PipelineBoardCard,
  PipelineBoardPendingReview,
} from "@/api/pipelineBoards";

afterEach(cleanup);

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

vi.mock("@/components/Runs/conversation/ReviewMergeCard", () => ({
  default: (props: {
    runId: string;
    message: {
      review?: { turns?: unknown[]; mergeInto?: string };
    };
    sourceOverride?: string | null;
    onResumed?: () => void;
  }) => (
    <div
      data-testid="guided-review"
      data-run-id={props.runId}
      data-turns={props.message.review?.turns?.length ?? 0}
      data-merge-into={props.message.review?.mergeInto ?? ""}
      data-source-null={props.sourceOverride === null ? "yes" : "no"}
    >
      <button type="button" onClick={props.onResumed}>
        Resolve guided review
      </button>
    </div>
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
    render(
      <SequentialReviews card={cardWithReviews(2)} onResolved={onResolved} />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Resolve" }));
    expect(onResolved).toHaveBeenCalledTimes(1);
  });

  it("routes guided AI reviews through the existing dialogue/merge card", () => {
    const onResolved = vi.fn();
    const card = cardWithReviews(1);
    const pending = card.pending_reviews?.[0];
    if (!pending) throw new Error("test requires one review");
    pending.instructions = "Play the candidate clip.";
    pending.review = {
      turns: [{ role: "companion", content: "Play the candidate clip." }],
      posture: "human_required",
      mergeStrategy: "squash",
      mergeInto: "main",
      maxTurns: 4,
    };

    render(<SequentialReviews card={card} onResolved={onResolved} />);

    const guided = screen.getByTestId("guided-review");
    expect(guided.getAttribute("data-run-id")).toBe(pending.run_id);
    expect(guided.getAttribute("data-turns")).toBe("1");
    expect(guided.getAttribute("data-merge-into")).toBe("main");
    expect(guided.getAttribute("data-source-null")).toBe("yes");
    expect(screen.queryByTestId("human-prompt")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Resolve guided review" }));
    expect(onResolved).toHaveBeenCalledTimes(1);
  });

  it("shows only the media attached to the active review turn", () => {
    const card = cardWithReviews(2);
    const first = card.pending_reviews?.[0];
    const second = card.pending_reviews?.[1];
    if (!first || !second) throw new Error("test requires two reviews");
    first.media = [
      {
        path: "renders/first.png",
        kind: "image",
        caption: "Validate first render",
      },
    ];
    second.media = [
      {
        path: "audio/second.wav",
        kind: "audio",
        caption: "Validate second mix",
      },
    ];

    render(<SequentialReviews card={card} onResolved={() => {}} />);
    expect(screen.getByText("Validate first render")).toBeTruthy();
    expect(screen.queryByText("Validate second mix")).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Next review" }));
    expect(screen.queryByText("Validate first render")).toBeNull();
    expect(screen.getByText("Validate second mix")).toBeTruthy();
    expect(screen.getAllByTestId("human-prompt")).toHaveLength(1);
  });

  it("shows workspace files above the exact AI-provided review points", () => {
    const card = cardWithReviews(1);
    const pending = card.pending_reviews?.[0];
    if (!pending) throw new Error("test requires one review");
    pending.questions = {
      review_image_path: "renders/review.png",
      plan_path: "plans/vertical-plan.json",
    };
    pending.instructions = [
      "# Validation du plan — plan-release-candidate-v2",
      "",
      "- `renders/review.png` : aperçu visuel ;",
      "- `plans/vertical-plan.json` : plan détaillé.",
      "",
      ...Array.from(
        { length: 30 },
        () => "Vérifiez chaque détail technique avant la validation.",
      ),
      "Approuvez uniquement si le résultat convient.",
    ].join("\n");
    pending.review_brief = {
      version: 1,
      source: "ai",
      points: [
        "Comparez le storyboard avec l’expérience cible.",
        "Confirmez que chaque étape produit un résultat jouable.",
      ],
    };

    render(<SequentialReviews card={card} onResolved={() => {}} />);

    const files = screen.getByRole("region", { name: "Files to review" });
    const question = screen.getByRole("region", { name: "Review question" });
    expect(
      files.compareDocumentPosition(question) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    expect(screen.getByRole("img", { name: "Aperçu visuel" })).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Next review file" }));
    expect(screen.getByText("vertical-plan.json")).toBeTruthy();
    expect(screen.getByText("Consignes de la revue IA")).toBeTruthy();
    expect(
      screen.getByText("Comparez le storyboard avec l’expérience cible."),
    ).toBeTruthy();
    expect(
      screen.getByText("Confirmez que chaque étape produit un résultat jouable."),
    ).toBeTruthy();
    expect(
      screen.queryByText("Consultez les fichiers présentés ci-dessus."),
    ).toBeNull();
    expect(screen.getByText(/Afficher les critères détaillés/)).toBeTruthy();
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
      <SequentialReviews
        card={{ ...cardWithReviews(3), pending_reviews: [c, a, b] }}
        onResolved={() => {}}
      />,
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
    view.rerender(
      <SequentialReviews
        card={{ ...cardWithReviews(3), pending_reviews: [b, c, a2] }}
        onResolved={() => {}}
      />,
    );
    expect(screen.getByTestId("human-prompt").getAttribute("data-run-id")).toBe(b.run_id);
    expect(
      (screen.getByRole("textbox", { name: "Review draft" }) as HTMLInputElement).value,
    ).toBe("draft for B");
    expect(screen.getByTestId("human-prompt").getAttribute("data-question")).not.toBe("A2?");

    // Once B is resolved/removed, the next oldest turn C gets the screen.
    // A2 appears only when the operator explicitly advances to its turn.
    view.rerender(
      <SequentialReviews
        card={{ ...cardWithReviews(3), pending_reviews: [c, a2] }}
        onResolved={() => {}}
      />,
    );
    expect(screen.getByTestId("human-prompt").getAttribute("data-run-id")).toBe(c.run_id);
    fireEvent.click(screen.getByRole("button", { name: "Next review" }));
    expect(screen.getByTestId("human-prompt").getAttribute("data-question")).toBe("A2?");
  });

  it("renders nothing when there are no pending reviews", () => {
    const card = { ...cardWithReviews(0), pending_reviews: [] };
    const html = renderToStaticMarkup(
      <SequentialReviews card={card} onResolved={() => {}} />,
    );
    expect(html).toBe("");
  });
});
