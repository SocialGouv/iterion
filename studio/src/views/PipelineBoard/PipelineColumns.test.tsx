import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it, vi } from "vitest";

import type { PipelineBoard, PipelineBoardCard } from "@/api/pipelineBoards";

vi.mock("@/api/runs", () => ({
  resumeRun: vi.fn(),
}));

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

vi.mock("@/components/Runs/conversation/HumanPromptForm", () => ({
  default: (props: {
    runId: string;
    nodeId: string;
    questions: Record<string, unknown>;
    sourceOverride?: string | null;
  }) => (
    <div
      data-testid="human-prompt"
      data-run-id={props.runId}
      data-node-id={props.nodeId}
      data-source-null={props.sourceOverride === null ? "yes" : "no"}
    >
      {JSON.stringify(props.questions)}
    </div>
  ),
}));

import { PipelineColumns } from "./PipelineColumns";

const columns = [
  { id: "todo", title: "Todo", kind: "todo" },
  { id: "in_progress", title: "In progress", kind: "in_progress" },
  { id: "done", title: "Done", kind: "done" },
  { id: "attention", title: "Attention", kind: "attention" },
];

function makeCard(partial: Partial<PipelineBoardCard>): PipelineBoardCard {
  return {
    id: "card",
    kind: "run",
    column_id: "todo",
    title: "Card",
    executed_nodes: 0,
    total_nodes: 0,
    tree_executed_nodes: 0,
    tree_total_nodes: 0,
    created_at: "2026-07-14T09:00:00Z",
    updated_at: "2026-07-14T10:00:00Z",
    ...partial,
  };
}

function makeBoard(cards: PipelineBoardCard[]): PipelineBoard {
  return {
    columns,
    cards,
    concurrency: { enabled: false, max: 0, active: 0, waiting: 0 },
  };
}

function render(board: PipelineBoard): string {
  return renderToStaticMarkup(
    <PipelineColumns board={board} onRefetch={() => {}} />,
  );
}

function countArticles(html: string): number {
  return (html.match(/role="article"/g) ?? []).length;
}

describe("PipelineColumns", () => {
  it("buckets cards client-side into the four fixed lanes", () => {
    const html = render(
      makeBoard([
        makeCard({ id: "t", column_id: "todo", kind: "task", title: "Todo card" }),
        makeCard({
          id: "p",
          column_id: "in_progress",
          title: "Running card",
          run_id: "r1",
          status: "running",
          tree_executed_nodes: 12,
          tree_total_nodes: 40,
        }),
        makeCard({
          id: "d",
          column_id: "done",
          title: "Done card",
          run_id: "r2",
          status: "finished",
          output: "Result text",
        }),
        makeCard({
          id: "a",
          column_id: "attention",
          title: "Broken card",
          run_id: "r3",
          status: "failed",
          error: "boom",
        }),
      ]),
    );

    // Lane-specific bodies only render when the card lands in the right lane:
    // the progress readout is in_progress-only, the <pre> output done-only.
    expect(html).toContain("Todo card");
    expect(html).toContain("Running card");
    expect(html).toContain("12 / 40 nodes");
    expect(html).toContain("Result text");
    expect(html).toContain("boom");
    expect(countArticles(html)).toBe(4);
    // Read-only projection — never draggable.
    expect(html).not.toContain('draggable="true"');
  });

  it("folds a paused descendant's review into the root's in_progress card", () => {
    const html = render(
      makeBoard([
        makeCard({
          id: "run:run-root",
          column_id: "in_progress",
          title: "Root pipeline",
          run_id: "run-root",
          status: "running",
          tree_executed_nodes: 5,
          tree_total_nodes: 10,
          descendant_count: 1,
          pending_reviews: [
            {
              run_id: "run-child",
              node_id: "approval",
              depth: 1,
              questions: { approved: "Ship it?" },
            },
          ],
        }),
      ]),
    );

    expect(html).toContain('data-run-id="run-child"');
    expect(html).toContain('data-node-id="approval"');
    expect(html).toContain('data-source-null="yes"');
    expect(html).toContain("Ship it?");
    // Descendants are folded — the child is NOT its own card.
    expect(countArticles(html)).toBe(1);
  });

  it("renders a node-weighted progress bar for a running root without reviews", () => {
    const html = render(
      makeBoard([
        makeCard({
          id: "run:r",
          column_id: "in_progress",
          title: "Running",
          run_id: "r",
          status: "running",
          tree_executed_nodes: 12,
          tree_total_nodes: 40,
          descendant_count: 2,
        }),
      ]),
    );

    expect(html).toContain("12 / 40 nodes");
    expect(html).toContain("+2 children");
    expect(html).not.toContain('data-testid="human-prompt"');
  });

  it("offers a resume affordance for a failed_resumable root in Attention", () => {
    const html = render(
      makeBoard([
        makeCard({
          id: "run:r",
          column_id: "attention",
          title: "Broken",
          run_id: "run-broken",
          status: "failed_resumable",
          error: "kaboom",
        }),
      ]),
    );

    expect(html).toContain("kaboom");
    expect(html).toContain("Resume run");
    expect(html).toContain("/runs/run-broken");
  });

  it("renders a queued run's entry input and waiting position in Todo", () => {
    const html = render(
      makeBoard([
        makeCard({
          id: "run:queued",
          column_id: "todo",
          kind: "run",
          title: "Queued run",
          run_id: "run-queued",
          queue_position: 3,
          entry_input: { area: "api" },
        }),
      ]),
    );

    expect(html).toContain("area:");
    expect(html).toContain("api");
    expect(html).toContain("#3");
  });
});
