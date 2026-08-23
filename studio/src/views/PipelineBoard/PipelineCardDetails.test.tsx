import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";

import type { PipelineBoardCard } from "@/api/pipelineBoards";

// ProducedElements pulls in react-query hooks; stub it so the composition
// test stays a pure render of the lane-conditional sections.
vi.mock("./ProducedElements", () => ({
  ProducedElements: (props: { runIds: string[]; status?: string }) => (
    <div
      data-testid="produced"
      data-run-ids={props.runIds.join(",")}
      data-status={props.status}
    />
  ),
}));

// SequentialReviews → HumanPromptForm reaches into the run store; stub the
// form the same way PipelineColumns.test does.
vi.mock("@/components/Runs/conversation/HumanPromptForm", () => ({
  default: (props: { runId: string; nodeId: string }) => (
    <div data-testid="human-prompt" data-run-id={props.runId} data-node-id={props.nodeId} />
  ),
}));

vi.mock("wouter", () => ({
  Link: ({
    href,
    children,
    ...rest
  }: React.AnchorHTMLAttributes<HTMLAnchorElement> & { href: string }) => (
    <a href={href} {...rest}>
      {children}
    </a>
  ),
}));

import { PipelineCardDetailsBody } from "./PipelineCardDetails";

function makeCard(partial: Partial<PipelineBoardCard>): PipelineBoardCard {
  return {
    id: "card",
    kind: "run",
    column_id: "in_progress",
    title: "Card",
    executed_nodes: 0,
    total_nodes: 0,
    tree_executed_nodes: 0,
    tree_total_nodes: 0,
    created_at: "2026-07-15T09:00:00Z",
    updated_at: "2026-07-15T10:00:00Z",
    ...partial,
  };
}

function render(card: PipelineBoardCard, stale = false): string {
  // The review panel fetches its change range, so the tree needs a query
  // client. retry:false settles the error path on the first rejection.
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return renderToStaticMarkup(
    <QueryClientProvider client={qc}>
      <PipelineCardDetailsBody card={card} stale={stale} onRefetch={() => {}} />
    </QueryClientProvider>,
  );
}

describe("PipelineCardDetailsBody", () => {
  it("Todo card shows inputs only — no produced elements, no response form", () => {
    const html = render(
      makeCard({
        column_id: "opened",
        kind: "task",
        issue_id: "iss-1",
        entry_input: { topic: "jazz", length: "3m" },
      }),
    );
    expect(html).toContain("Inputs");
    expect(html).toContain("topic");
    expect(html).toContain("jazz");
    expect(html).not.toContain('data-testid="produced"');
    expect(html).not.toContain('data-testid="human-prompt"');
    expect(html).not.toContain("Response required");
  });

  it("In progress card shows inputs + produced elements, no form", () => {
    const html = render(
      makeCard({
        column_id: "in_progress",
        run_id: "run-42",
        status: "running",
        entry_input: { topic: "jazz" },
      }),
    );
    expect(html).toContain("Inputs");
    expect(html).toContain('data-testid="produced"');
    expect(html).toContain('data-run-ids="run-42"');
    expect(html).toContain('data-status="running"');
    expect(html).not.toContain('data-testid="human-prompt"');
  });

  it("aggregates produced elements across the whole run tree (sub-bots)", () => {
    const html = render(
      makeCard({
        column_id: "in_progress",
        run_id: "run-root",
        status: "running",
        tree_run_ids: ["run-root", "run-child"],
      }),
    );
    expect(html).toContain('data-run-ids="run-root,run-child"');
  });

  it("In progress + pending review shows the response form and produced elements", () => {
    const html = render(
      makeCard({
        column_id: "in_progress",
        run_id: "run-root",
        status: "running",
        entry_input: { topic: "jazz" },
        pending_reviews: [
          {
            run_id: "run-child",
            node_id: "approval",
            depth: 1,
            updated_at: "2026-07-15T09:30:00Z",
            questions: { approved: "Ship it?" },
          },
        ],
      }),
    );
    expect(html).toContain("Response required");
    expect(html).toContain('data-testid="human-prompt"');
    expect(html).toContain('data-run-id="run-child"');
    expect(html).toContain('data-node-id="approval"');
    expect(html).toContain('data-testid="produced"');
    expect(html).toContain("Inputs");
  });

  it("an interrupted blocked pipeline gets the explanation banner next to its review form", () => {
    const html = render(
      makeCard({
        column_id: "in_progress",
        run_id: "run-orphan",
        status: "failed_resumable",
        failed: true,
        pending_reviews: [
          {
            run_id: "c1",
            node_id: "review",
            depth: 1,
            updated_at: "2026-07-15T09:30:00Z",
          },
        ],
      }),
    );
    expect(html).toContain("Response required");
    expect(html).toContain("interrupted");
    expect(html).toContain('data-testid="human-prompt"');
  });

  it("a healthy blocked pipeline has no interrupted banner", () => {
    const html = render(
      makeCard({
        column_id: "in_progress",
        run_id: "run-ok",
        status: "running",
        pending_reviews: [
          {
            run_id: "c1",
            node_id: "review",
            depth: 1,
            updated_at: "2026-07-15T09:30:00Z",
          },
        ],
      }),
    );
    expect(html).toContain("Response required");
    expect(html).not.toContain("interrupted");
  });

  it("Failed card shows the failure reason + inputs + produced elements", () => {
    const html = render(
      makeCard({
        column_id: "closed",
        run_id: "run-ko",
        status: "failed_resumable",
        failed: true,
        error: "budget exceeded at node compose",
        entry_input: { topic: "jazz" },
      }),
    );
    expect(html).toContain("Failure");
    expect(html).toContain("budget exceeded at node compose");
    expect(html).toContain("Inputs");
    expect(html).toContain('data-testid="produced"');
    expect(html).toContain('data-run-ids="run-ko"');
  });

  it("a given-up card names the dispatcher as the one who filed the ticket", () => {
    // The ticket reads terminal ("blocked") while the card sits in Needs
    // attention. Without naming who filed it, the operator reads that state
    // as somebody's decision and stops looking.
    const html = render(
      makeCard({
        column_id: "needs_attention",
        run_id: "run-dead",
        issue_id: "iss-9",
        issue_state: "blocked",
        status: "failed_resumable",
        failed: true,
        error: "kaboom",
        gave_up: { run_id: "run-dead", state: "blocked", attempts: 3 },
      }),
    );
    expect(html).toContain("The dispatcher gave up after 3 attempts");
    expect(html).toContain("kaboom"); // the underlying reason stays
  });

  it("successful Closed card shows inputs + result + produced elements", () => {
    const html = render(
      makeCard({
        column_id: "closed",
        run_id: "run-done",
        status: "finished",
        entry_input: { topic: "jazz" },
        output: "final track rendered",
      }),
    );
    expect(html).toContain("Inputs");
    expect(html).toContain("Result");
    expect(html).toContain("final track rendered");
    expect(html).toContain('data-testid="produced"');
  });

  it("renders object input values as pretty-printed JSON blocks, scalars inline", () => {
    const html = render(
      makeCard({
        column_id: "opened",
        kind: "task",
        entry_input: { topic: "jazz", options: { tempo: 120, mood: "calm" } },
      }),
    );
    // Structured value → <pre> with indented JSON (not a single-line blob).
    expect(html).toContain("<pre");
    expect(html).toContain("&quot;tempo&quot;: 120");
    expect(html).not.toContain("{&quot;tempo&quot;:120");
    // Scalar sibling keeps the plain rendering.
    expect(html).toContain("jazz");
  });

  it("every input value carries a copy button, and JSON a raw/pretty toggle", () => {
    const html = render(
      makeCard({
        column_id: "opened",
        kind: "task",
        entry_input: { topic: "jazz", options: { tempo: 120 } },
      }),
    );
    expect(html).toContain('aria-label="Copy topic"');
    expect(html).toContain('aria-label="Copy options"');
    // Only the structured value has a second form to toggle to.
    expect(html).toContain('aria-label="Show raw options"');
    expect(html).not.toContain('aria-label="Show raw topic"');
  });

  it("a long input value collapses to a preview with an in-place expand toggle", () => {
    const brief = Array.from({ length: 40 }, (_, i) => `paragraph ${i}`).join("\n");
    const html = render(
      makeCard({ column_id: "opened", kind: "task", entry_input: { brief } }),
    );
    // Nothing is truncated away — the full text ships, only its height is capped.
    expect(html).toContain("paragraph 39");
    expect(html).toContain("max-height:12rem");
    expect(html).toContain("Show all 40 lines");
  });

  it("a short input value renders whole, with no expand toggle", () => {
    const html = render(
      makeCard({ column_id: "opened", kind: "task", entry_input: { topic: "jazz" } }),
    );
    expect(html).not.toContain("max-height");
    expect(html).not.toContain("Show all");
  });

  it("the contract rows are expandable/copyable values too", () => {
    const html = render(
      makeCard({
        column_id: "opened",
        kind: "task",
        entry_input: { feature_id: "feat-42", doc_refs: ["docs/a.md", "docs/b.md"] },
      }),
    );
    expect(html).toContain("Ticket contract");
    expect(html).toContain("feat-42");
    expect(html).toContain('aria-label="Copy feature_id"');
    expect(html).toContain('aria-label="Copy doc_refs"');
  });

  it("a long result collapses at a taller preview than an input", () => {
    const output = Array.from({ length: 60 }, (_, i) => `answer line ${i}`).join("\n");
    const html = render(
      makeCard({
        column_id: "closed",
        run_id: "run-done",
        status: "finished",
        output,
      }),
    );
    expect(html).toContain("Result");
    expect(html).toContain("answer line 59");
    expect(html).toContain("max-height:24rem");
    expect(html).toContain("Show all 60 lines");
    expect(html).toContain('aria-label="Copy result"');
  });

  it("renders a 'No inputs recorded' fallback and a stale banner", () => {
    const html = render(makeCard({ column_id: "opened", kind: "task" }), true);
    expect(html).toContain("No additional inputs");
    expect(html).toContain("no longer on the board");
  });
});

describe("InputsList image carousel", () => {
  it("renders a JSON list of image paths as a carousel of workspace-image URLs", () => {
    const html = render(
      makeCard({
        column_id: "opened",
        entry_input: {
          character: "Boudicca",
          character_refs:
            '["assets/characters/histoire/boudicca/refs/master.png", "assets/characters/histoire/boudicca/refs/full_body.png"]',
        },
      }),
    );
    expect(html).toContain(
      "/api/v1/pipeline-board/workspace-images/assets/characters/histoire/boudicca/refs/master.png",
    );
    // One image at a time, with a position counter and cycling controls.
    expect(html).toContain("1/2");
    expect(html).toContain("Next image");
    expect(html).toContain("Previous image");
    // Sibling non-image values keep the plain monospace rendering.
    expect(html).toContain("Boudicca");
  });

  it("renders a single bare image path as an image without cycling controls", () => {
    const html = render(
      makeCard({
        column_id: "opened",
        entry_input: { cover: "assets/cover art/../covers/final.png" },
      }),
    );
    expect(html).not.toContain("workspace-images");
    // Path with whitespace stays plain text; a clean single path renders.
    const clean = render(
      makeCard({
        column_id: "opened",
        entry_input: { cover: "assets/covers/final.png" },
      }),
    );
    expect(clean).toContain("/api/v1/pipeline-board/workspace-images/assets/covers/final.png");
    expect(clean).not.toContain("Next image");
  });

  it("keeps sentences and mixed arrays as plain text", () => {
    const html = render(
      makeCard({
        column_id: "opened",
        entry_input: {
          notes: "voir le rendu final dans exports/preview.png",
          mixed: '["assets/refs/master.png", "pas une image"]',
        },
      }),
    );
    expect(html).not.toContain("workspace-images");
    expect(html).toContain("voir le rendu final");
  });
});
