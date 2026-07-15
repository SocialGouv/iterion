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
  return renderToStaticMarkup(
    <PipelineCardDetailsBody card={card} stale={stale} onRefetch={() => {}} />,
  );
}

describe("PipelineCardDetailsBody", () => {
  it("Todo card shows inputs only — no produced elements, no response form", () => {
    const html = render(
      makeCard({
        column_id: "todo",
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
          { run_id: "run-child", node_id: "approval", depth: 1, questions: { approved: "Ship it?" } },
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

  it("Done card shows inputs + result + produced elements", () => {
    const html = render(
      makeCard({
        column_id: "done",
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

  it("renders a 'No inputs recorded' fallback and a stale banner", () => {
    const html = render(makeCard({ column_id: "todo", kind: "task" }), true);
    expect(html).toContain("No inputs recorded");
    expect(html).toContain("no longer on the board");
  });
});
