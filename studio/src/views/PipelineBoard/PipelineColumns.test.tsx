import { renderToStaticMarkup } from "react-dom/server";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { PipelineBoard, PipelineBoardCard } from "@/api/pipelineBoards";

// PipelineColumns imports markPipelineTaskReady for the button-driven
// Draft ↔ Todo moves.
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
  canMoveToDraft,
  canMoveToTodo,
  isTicketEditable,
  moveTicket,
} from "./PipelineColumns";

const columns = [
  { id: "draft", title: "Draft", kind: "draft" },
  { id: "todo", title: "Todo", kind: "todo" },
  { id: "in_progress", title: "In progress", kind: "in_progress" },
  { id: "done", title: "Done", kind: "done" },
  { id: "failed", title: "Failed", kind: "failed" },
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

    expect(html).toContain("Draft task");
    expect(html).toContain("Queued run");
    expect(html).toContain("#2"); // queue position badge (Todo)
    expect(html).toContain("12 / 40 nodes"); // progress (in_progress)
    expect(count(html, 'role="article"')).toBe(4);
    // Drag & drop is gone entirely.
    expect(html).not.toContain('draggable="true"');
  });

  it("cards are lean: no body text, labels, inputs, kind badge, or output", () => {
    const html = render(
      makeBoard([
        makeCard({
          id: "d",
          column_id: "draft",
          kind: "task",
          issue_id: "iss-1",
          title: "Draft task",
          body: "long ticket description",
          labels: ["labelled-x"],
          entry_input: { topic: "jazz-input" },
          priority: 3,
        }),
        makeCard({
          id: "done",
          column_id: "done",
          run_id: "r3",
          status: "finished",
          title: "Done card",
          output: "Result text",
          workflow_name: "wf_name_meta",
          bot_id: "bot-meta",
        }),
      ]),
    );

    expect(html).not.toContain("long ticket description");
    expect(html).not.toContain("labelled-x");
    expect(html).not.toContain("jazz-input");
    expect(html).not.toContain("Result text"); // output lives in the sidebar
    expect(html).not.toContain("bot-meta"); // meta chips removed
    expect(html).not.toContain("wf_name_meta");
  });

  it("renders a failed pipeline in the Failed lane with its reason and Retry", () => {
    const html = render(
      makeBoard([
        makeCard({
          id: "failed",
          column_id: "failed",
          kind: "run",
          title: "Broke last time",
          issue_id: "iss-2",
          run_id: "run-old",
          status: "failed_resumable",
          failed: true,
          error: "kaboom",
        }),
      ]),
    );

    expect(html).toContain("kaboom"); // the reason stays visible
    expect(html).toContain("Retry"); // ticket-backed → retryable to Todo
  });

  it("a failed pipeline without a ticket shows the reason but no Retry", () => {
    const html = render(
      makeBoard([
        makeCard({
          id: "failed-standalone",
          column_id: "failed",
          kind: "run",
          title: "Manual run",
          run_id: "run-solo",
          status: "failed",
          failed: true,
          error: "exploded",
        }),
      ]),
    );
    expect(html).toContain("exploded");
    expect(html).not.toContain("Retry");
  });

  it("shows a Blocked tag naming the gate + progress; the form lives in the sidebar only", () => {
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

    // The blocked tag names the pending gate…
    expect(html).toContain("Blocked — human review · approval");
    expect(html).not.toContain("Ship it?"); // no inline form
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

describe("priority", () => {
  it("shows the P badge on Draft / Todo / Failed cards (the launch-order dial)", () => {
    const html = render(
      makeBoard([
        makeCard({ id: "d", column_id: "draft", kind: "task", issue_id: "iss-1", priority: 5 }),
        makeCard({ id: "t", column_id: "todo", kind: "task", issue_id: "iss-2", priority: 3 }),
        makeCard({
          id: "f",
          column_id: "failed",
          kind: "run",
          issue_id: "iss-3",
          run_id: "r1",
          status: "failed",
          failed: true,
          priority: 2,
        }),
      ]),
    );
    expect(html).toContain("P5");
    expect(html).toContain("P3");
    expect(html).toContain("P2");
    expect(html).toContain("launch first from Todo");
  });

  it("hides the badge for unprioritized (0) cards", () => {
    const html = render(
      makeBoard([
        makeCard({ id: "d", column_id: "todo", kind: "task", issue_id: "iss-1", priority: 0 }),
      ]),
    );
    expect(html).not.toContain(">P0<");
  });
});

describe("card details affordance", () => {
  it("renders a Details affordance and a pointer cursor when onOpenCard is given", () => {
    const html = renderToStaticMarkup(
      <PipelineColumns
        board={makeBoard([
          makeCard({ id: "a", column_id: "in_progress", run_id: "r1", status: "running", title: "Running" }),
        ])}
        onRefetch={() => {}}
        onOpenCard={() => {}}
      />,
    );
    expect(html).toContain("Details for Running");
    expect(html).toContain("cursor-pointer");
    expect(html).not.toContain("cursor-grab");
  });

  it("omits the Details affordance when onOpenCard is absent", () => {
    const html = render(
      makeBoard([
        makeCard({ id: "a", column_id: "in_progress", run_id: "r1", status: "running", title: "Running" }),
      ]),
    );
    expect(html).not.toContain("aria-label=\"Details for");
    expect(html).not.toContain("cursor-pointer");
  });
});

describe("move buttons (no drag & drop)", () => {
  it("a Draft task shows → Todo; a Todo task shows → Draft", () => {
    const html = render(
      makeBoard([
        makeCard({ id: "d", column_id: "draft", kind: "task", issue_id: "iss-1", title: "Prep me" }),
        makeCard({ id: "t", column_id: "todo", kind: "task", issue_id: "iss-2", title: "Staged" }),
      ]),
    );
    expect(html).toContain("→ Todo");
    expect(html).toContain("→ Draft");
  });

  it("run-backed cards get no move buttons", () => {
    const html = render(
      makeBoard([
        makeCard({ id: "q", column_id: "todo", kind: "run", run_id: "r1", status: "queued", issue_id: "iss-3" }),
        makeCard({ id: "p", column_id: "in_progress", kind: "run", run_id: "r2", status: "running" }),
        makeCard({ id: "done", column_id: "done", kind: "run", run_id: "r3", status: "finished" }),
      ]),
    );
    expect(html).not.toContain("→ Todo");
    expect(html).not.toContain("→ Draft");
  });
});

describe("move helpers", () => {
  it("canMoveToTodo: draft tasks stage; failed-lane tickets retry", () => {
    expect(canMoveToTodo(makeCard({ column_id: "draft", kind: "task", issue_id: "iss-1" }))).toBe(true);
    expect(
      canMoveToTodo(makeCard({ column_id: "failed", kind: "run", issue_id: "iss-1", failed: true })),
    ).toBe(true);
    // Legacy tolerance: a failed card still projected into draft stays retryable.
    expect(
      canMoveToTodo(makeCard({ column_id: "draft", kind: "run", issue_id: "iss-1", failed: true })),
    ).toBe(true);
    // Not draft/failed lane, executing, or no ticket → immobile.
    expect(canMoveToTodo(makeCard({ column_id: "todo", kind: "task", issue_id: "iss-1" }))).toBe(false);
    expect(
      canMoveToTodo(makeCard({ column_id: "draft", kind: "run", issue_id: "iss-1", status: "running" })),
    ).toBe(false);
    expect(canMoveToTodo(makeCard({ column_id: "draft", kind: "task" }))).toBe(false);
    expect(canMoveToTodo(makeCard({ column_id: "failed", kind: "run", failed: true }))).toBe(false);
  });

  it("canMoveToDraft: todo-only, unlaunched task-backed tickets", () => {
    expect(canMoveToDraft(makeCard({ column_id: "todo", kind: "task", issue_id: "iss-1" }))).toBe(true);
    // A queued RUN in Todo is fixed by run state.
    expect(
      canMoveToDraft(makeCard({ column_id: "todo", kind: "run", issue_id: "iss-1", status: "queued" })),
    ).toBe(false);
    expect(canMoveToDraft(makeCard({ column_id: "draft", kind: "task", issue_id: "iss-1" }))).toBe(false);
    expect(canMoveToDraft(makeCard({ column_id: "todo", kind: "task" }))).toBe(false);
  });

  it("moveTicket(true) stages to Todo, then refetches", async () => {
    markReadyMock.mockResolvedValue(undefined);
    const onDone = vi.fn();
    await moveTicket("iss-1", true, onDone);
    expect(markReadyMock).toHaveBeenCalledWith("iss-1", true);
    expect(onDone).toHaveBeenCalledTimes(1);
  });

  it("moveTicket(false) unstages to Draft", async () => {
    markReadyMock.mockResolvedValue(undefined);
    const onDone = vi.fn();
    await moveTicket("iss-1", false, onDone);
    expect(markReadyMock).toHaveBeenCalledWith("iss-1", false);
    expect(onDone).toHaveBeenCalledTimes(1);
  });
});

describe("isTicketEditable", () => {
  it("allows editing not-yet-run task-backed tickets (incl. failed-lane fixes)", () => {
    expect(
      isTicketEditable(makeCard({ column_id: "draft", kind: "task", issue_id: "iss-1" })),
    ).toBe(true);
    expect(
      isTicketEditable(
        makeCard({ column_id: "failed", kind: "run", issue_id: "iss-2", failed: true }),
      ),
    ).toBe(true);
    expect(
      isTicketEditable(makeCard({ column_id: "todo", kind: "task", issue_id: "iss-3" })),
    ).toBe(true);
  });

  it("blocks editing executing / finished / queued cards", () => {
    expect(
      isTicketEditable(
        makeCard({ column_id: "in_progress", kind: "run", issue_id: "iss-4", status: "running" }),
      ),
    ).toBe(false);
    expect(
      isTicketEditable(
        makeCard({ column_id: "done", kind: "run", issue_id: "iss-5", status: "finished" }),
      ),
    ).toBe(false);
    expect(
      isTicketEditable(
        makeCard({ column_id: "todo", kind: "run", issue_id: "iss-6", status: "queued" }),
      ),
    ).toBe(false);
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
