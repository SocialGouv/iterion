import { renderToStaticMarkup } from "react-dom/server";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { PipelineBoard, PipelineBoardCard } from "@/api/pipelineBoards";

// PipelineColumns imports markPipelineTaskReady for the Draft ↔ Todo drop.
const { markReadyMock } = vi.hoisted(() => ({ markReadyMock: vi.fn() }));
vi.mock("@/api/pipelineBoards", () => ({
  markPipelineTaskReady: markReadyMock,
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

import {
  PipelineColumns,
  dropTicketToColumn,
  isTicketDraggable,
  isTicketEditable,
  readyStateForDropColumn,
} from "./PipelineColumns";

const columns = [
  { id: "draft", title: "Draft", kind: "draft" },
  { id: "todo", title: "Todo", kind: "todo" },
  { id: "in_progress", title: "In progress", kind: "in_progress" },
  { id: "done", title: "Done", kind: "done" },
];

function makeCard(partial: Partial<PipelineBoardCard>): PipelineBoardCard {
  return {
    id: "card",
    kind: "run",
    column_id: "draft",
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

function count(html: string, needle: string): number {
  return html.split(needle).length - 1;
}

beforeEach(() => {
  markReadyMock.mockReset();
});

describe("PipelineColumns", () => {
  it("buckets cards client-side into the four fixed lanes", () => {
    const html = render(
      makeBoard([
        makeCard({ id: "d", column_id: "draft", kind: "task", issue_id: "iss-1", title: "Draft task" }),
        makeCard({
          id: "t",
          column_id: "todo",
          title: "Queued run",
          run_id: "r1",
          status: "queued",
          queue_position: 2,
        }),
        makeCard({
          id: "p",
          column_id: "in_progress",
          title: "Running card",
          run_id: "r2",
          status: "running",
          tree_executed_nodes: 12,
          tree_total_nodes: 40,
        }),
        makeCard({
          id: "done",
          column_id: "done",
          title: "Done card",
          run_id: "r3",
          status: "finished",
          output: "Result text",
        }),
      ]),
    );

    // Lane-specific bodies only render when the card lands in the right lane.
    expect(html).toContain("Draft task");
    expect(html).toContain("Queued run");
    expect(html).toContain("#2"); // queue position badge (Todo)
    expect(html).toContain("12 / 40 nodes"); // progress (in_progress)
    expect(html).toContain("Result text"); // output (done)
    expect(count(html, 'role="article"')).toBe(4);
    // Only the draft task ticket is draggable.
    expect(count(html, 'draggable="true"')).toBe(1);
  });

  it("renders a failed ticket in Draft with a Failed badge and error, draggable for retry", () => {
    const html = render(
      makeBoard([
        makeCard({
          id: "failed",
          column_id: "draft",
          kind: "run",
          title: "Broke last time",
          issue_id: "iss-2",
          run_id: "run-old",
          failed: true,
          error: "kaboom",
        }),
      ]),
    );

    expect(html).toContain("Failed");
    expect(html).toContain("kaboom");
    // A failed ticket is draggable (drop into Todo = retry).
    expect(count(html, 'draggable="true"')).toBe(1);
  });

  it("does not make an executing run card draggable", () => {
    const html = render(
      makeBoard([
        makeCard({
          id: "running",
          column_id: "in_progress",
          kind: "run",
          title: "Running",
          issue_id: "iss-3", // has a ticket, but is executing
          run_id: "run-x",
          status: "running",
          tree_executed_nodes: 1,
          tree_total_nodes: 4,
        }),
      ]),
    );

    expect(html).not.toContain('draggable="true"');
  });

  it("shows a Blocked tag + progress for pending reviews — the form lives in the sidebar only", () => {
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

    // The blocked tag replaces the inline review form…
    expect(html).toContain("Blocked — human review");
    expect(html).not.toContain('data-testid="human-prompt"');
    expect(html).not.toContain("Ship it?");
    // …while the tree progress stays visible.
    expect(html).toContain("5 / 10 nodes");
    expect(count(html, 'role="article"')).toBe(1); // child is folded, not its own card
  });

  it("pluralizes the Blocked tag for several pending reviews", () => {
    const html = render(
      makeBoard([
        makeCard({
          id: "run:multi",
          column_id: "in_progress",
          run_id: "run-multi",
          status: "running",
          tree_executed_nodes: 2,
          tree_total_nodes: 8,
          pending_reviews: [
            { run_id: "c1", node_id: "review", depth: 1 },
            { run_id: "c2", node_id: "review", depth: 1 },
            { run_id: "c3", node_id: "review", depth: 1 },
          ],
        }),
      ]),
    );
    expect(html).toContain("Blocked — 3 human reviews");
    expect(html).toContain("2 / 8 nodes");
  });
});

describe("card details affordance", () => {
  it("renders a Details affordance for every card when onOpenCard is given", () => {
    const html = renderToStaticMarkup(
      <PipelineColumns
        board={makeBoard([
          makeCard({ id: "a", column_id: "in_progress", run_id: "r1", status: "running", title: "Running" }),
          makeCard({ id: "b", column_id: "done", run_id: "r2", status: "finished", title: "Done" }),
        ])}
        onRefetch={() => {}}
        onOpenCard={() => {}}
      />,
    );
    expect(html).toContain("Details for Running");
    expect(html).toContain("Details for Done");
  });

  it("omits the Details affordance when onOpenCard is absent", () => {
    const html = render(
      makeBoard([
        makeCard({ id: "a", column_id: "in_progress", run_id: "r1", status: "running", title: "Running" }),
      ]),
    );
    expect(html).not.toContain("aria-label=\"Details for");
  });
});

describe("drag-and-drop helpers", () => {
  it("only allows dragging non-executing task-backed tickets", () => {
    expect(isTicketDraggable(makeCard({ kind: "task", issue_id: "iss-1" }))).toBe(true);
    expect(
      isTicketDraggable(makeCard({ kind: "run", issue_id: "iss-1", failed: true })),
    ).toBe(true);
    // Running / queued / finished runs are fixed by run state.
    expect(
      isTicketDraggable(makeCard({ kind: "run", issue_id: "iss-1", status: "running" })),
    ).toBe(false);
    // A task with no tracker issue can't be moved between states.
    expect(isTicketDraggable(makeCard({ kind: "task" }))).toBe(false);
  });

  it("maps drop columns to the ready flag", () => {
    expect(readyStateForDropColumn("todo")).toBe(true);
    expect(readyStateForDropColumn("draft")).toBe(false);
    expect(readyStateForDropColumn("in_progress")).toBeNull();
    expect(readyStateForDropColumn("done")).toBeNull();
  });

  it("dropping into Todo marks the ticket ready, then refetches", async () => {
    markReadyMock.mockResolvedValue(undefined);
    const onDone = vi.fn();
    await dropTicketToColumn("iss-1", "todo", onDone);
    expect(markReadyMock).toHaveBeenCalledWith("iss-1", true);
    expect(onDone).toHaveBeenCalledTimes(1);
  });

  it("dropping into Draft unmarks the ticket", async () => {
    markReadyMock.mockResolvedValue(undefined);
    const onDone = vi.fn();
    await dropTicketToColumn("iss-1", "draft", onDone);
    expect(markReadyMock).toHaveBeenCalledWith("iss-1", false);
    expect(onDone).toHaveBeenCalledTimes(1);
  });

  it("ignores drops on non-target columns", async () => {
    const onDone = vi.fn();
    await dropTicketToColumn("iss-1", "in_progress", onDone);
    expect(markReadyMock).not.toHaveBeenCalled();
    expect(onDone).not.toHaveBeenCalled();
  });
});

describe("isTicketEditable", () => {
  it("allows editing not-yet-run task-backed tickets", () => {
    // Draft task card.
    expect(
      isTicketEditable(makeCard({ column_id: "draft", kind: "task", issue_id: "iss-1" })),
    ).toBe(true);
    // Draft failed ticket (a failed run being retried) is still editable.
    expect(
      isTicketEditable(
        makeCard({ column_id: "draft", kind: "run", issue_id: "iss-2", failed: true }),
      ),
    ).toBe(true);
    // Ready-but-unlaunched Todo task card.
    expect(
      isTicketEditable(makeCard({ column_id: "todo", kind: "task", issue_id: "iss-3" })),
    ).toBe(true);
  });

  it("blocks editing executing / finished / queued cards", () => {
    // Running run.
    expect(
      isTicketEditable(
        makeCard({ column_id: "in_progress", kind: "run", issue_id: "iss-4", status: "running" }),
      ),
    ).toBe(false);
    // Done run.
    expect(
      isTicketEditable(
        makeCard({ column_id: "done", kind: "run", issue_id: "iss-5", status: "finished" }),
      ),
    ).toBe(false);
    // Queued run sitting in Todo (kind run, not a task).
    expect(
      isTicketEditable(
        makeCard({ column_id: "todo", kind: "run", issue_id: "iss-6", status: "queued" }),
      ),
    ).toBe(false);
    // A task with no tracker issue can't be patched.
    expect(isTicketEditable(makeCard({ column_id: "draft", kind: "task" }))).toBe(false);
  });

  it("renders an Edit affordance only for editable cards when a handler is given", () => {
    const editableHtml = renderToStaticMarkup(
      <PipelineColumns
        board={makeBoard([
          makeCard({ id: "e", column_id: "draft", kind: "task", issue_id: "iss-1", title: "Draft task" }),
        ])}
        onRefetch={() => {}}
        onEditTask={() => {}}
      />,
    );
    expect(editableHtml).toContain("Edit ticket Draft task");

    const runningHtml = renderToStaticMarkup(
      <PipelineColumns
        board={makeBoard([
          makeCard({ id: "r", column_id: "in_progress", kind: "run", run_id: "run-x", status: "running" }),
        ])}
        onRefetch={() => {}}
        onEditTask={() => {}}
      />,
    );
    expect(runningHtml).not.toContain("aria-label=\"Edit ticket");
  });
});
