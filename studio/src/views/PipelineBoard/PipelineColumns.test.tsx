import { renderToStaticMarkup } from "react-dom/server";
import { beforeEach, describe, expect, it, vi } from "vitest";

import type { PipelineBoard, PipelineBoardCard } from "@/api/pipelineBoards";

// PipelineColumns imports markPipelineTaskReady for the button-driven ready
// toggles, plus the delete/reset ticket actions and the run controls
// (pause / resume / stop).
const { markReadyMock, deleteTaskMock, resetTaskMock, launchTaskMock, pauseRunMock, resumeRunMock, cancelRunMock } =
  vi.hoisted(() => ({
    markReadyMock: vi.fn(),
    deleteTaskMock: vi.fn(),
    resetTaskMock: vi.fn(),
    launchTaskMock: vi.fn(),
    pauseRunMock: vi.fn(),
    resumeRunMock: vi.fn(),
    cancelRunMock: vi.fn(),
  }));
vi.mock("@/api/pipelineBoards", () => ({
  markPipelineTaskReady: markReadyMock,
  deletePipelineTask: deleteTaskMock,
  resetPipelineTask: resetTaskMock,
  launchPipelineTask: launchTaskMock,
}));
vi.mock("@/api/runs", () => ({
  pauseRun: pauseRunMock,
  resumeRun: resumeRunMock,
  cancelRun: cancelRunMock,
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
  useLocation: () => ["/pipelines", vi.fn()],
}));

// The card resolves "Edit bot" against the catalog store.
vi.mock("@/store/bots", () => ({
  useBotsStore: (sel: (s: unknown) => unknown) =>
    sel({
      bots: [
        { name: "shorts-episode", rel_path: "bots/shorts-episode", is_bundle: true },
      ],
      fetch: () => Promise.resolve(),
    }),
}));

vi.mock("@/store/ui", () => ({
  useUIStore: (sel: (s: { addToast: () => void }) => unknown) =>
    sel({ addToast: vi.fn() }),
}));

import { cardReady, closedOutcome } from "./columnFilters";
import { resolveMenuItems, resolvePrimaryAction } from "./primaryAction";
import {
  PipelineColumns,
  LAUNCH_DRAG_TYPE,
  botEditorPath,
  canDeleteTicket,
  canLaunchNow,
  canMarkReady,
  canPauseRun,
  canResetTicket,
  canResumeRun,
  canStopRun,
  canUnmarkReady,
  isTicketEditable,
  moveTicket,
} from "./PipelineColumns";
import { resumePipelineRun } from "./resumePipelineRun";

// Three fixed lanes: Opened (backlog + ready staging), In progress, Closed
// (success + failure).
const columns = [
  { id: "opened", title: "Opened", kind: "opened" },
  { id: "in_progress", title: "In progress", kind: "in_progress" },
  { id: "closed", title: "Closed", kind: "closed" },
];

function makeCard(partial: Partial<PipelineBoardCard>): PipelineBoardCard {
  return {
    id: "card",
    kind: "run",
    column_id: "opened",
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
  deleteTaskMock.mockReset();
  resetTaskMock.mockReset();
  pauseRunMock.mockReset();
  resumeRunMock.mockReset();
  cancelRunMock.mockReset();
});

describe("resumePipelineRun", () => {
  it("resumes normally when the workflow source is unchanged", async () => {
    resumeRunMock.mockResolvedValueOnce({ run_id: "r1", status: "running" });

    await resumePipelineRun("r1", vi.fn());

    expect(resumeRunMock).toHaveBeenCalledOnce();
    expect(resumeRunMock).toHaveBeenCalledWith("r1", {});
  });

  it("asks before retrying a stale run with the current workflow", async () => {
    resumeRunMock
      .mockRejectedValueOnce(
        new Error("runtime: workflow source has changed since this run was started"),
      )
      .mockResolvedValueOnce({ run_id: "r1", status: "running" });
    const confirmUpdatedWorkflow = vi.fn().mockResolvedValue(true);

    await resumePipelineRun("r1", confirmUpdatedWorkflow);

    expect(confirmUpdatedWorkflow).toHaveBeenCalledOnce();
    expect(resumeRunMock).toHaveBeenNthCalledWith(1, "r1", {});
    expect(resumeRunMock).toHaveBeenNthCalledWith(2, "r1", { force: true });
  });

  it("does not force-resume when the operator declines", async () => {
    resumeRunMock.mockRejectedValueOnce(
      new Error("runtime: workflow source has changed since this run was started"),
    );

    await resumePipelineRun("r1", vi.fn().mockResolvedValue(false));

    expect(resumeRunMock).toHaveBeenCalledOnce();
  });

  it("does not hide unrelated resume errors", async () => {
    const error = new Error("cannot be resumed (status: finished)");
    resumeRunMock.mockRejectedValueOnce(error);
    const confirmUpdatedWorkflow = vi.fn();

    await expect(
      resumePipelineRun("r1", confirmUpdatedWorkflow),
    ).rejects.toBe(error);
    expect(confirmUpdatedWorkflow).not.toHaveBeenCalled();
  });
});

describe("PipelineColumns", () => {
  it("buckets cards client-side into the three fixed lanes", () => {
    const html = render(
      makeBoard([
        makeCard({ id: "d", column_id: "opened", kind: "task", issue_id: "iss-1", title: "Draft task" }),
        makeCard({
          id: "t",
          column_id: "opened",
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
          column_id: "closed",
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
    // Card POSITION stays server-derived: the only drag on this board is the
    // launch-now override, and only launchable Opened tickets carry it (here:
    // the draft; not the queued run, the live run, or the finished one).
    expect(count(html, 'draggable="true"')).toBe(1);
  });

  it("cards are lean: no body text, raw inputs, or output; tags are visible chips", () => {
    const html = render(
      makeBoard([
        makeCard({
          id: "d",
          column_id: "opened",
          kind: "task",
          issue_id: "iss-1",
          title: "Lean task",
          body: "long ticket description",
          labels: ["labelled-x"],
          entry_input: { topic: "jazz-input", character: "Boudicca" },
          priority: 3,
        }),
        makeCard({
          id: "done",
          column_id: "closed",
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
    expect(html).not.toContain("jazz-input"); // raw input keys stay off the face
    expect(html).not.toContain("Result text"); // output lives in the sidebar
    expect(html).not.toContain("bot-meta"); // meta chips removed
    expect(html).not.toContain("wf_name_meta");
    // Tags (issue labels + content-derived) ARE on the card face.
    expect(html).toContain("labelled-x");
    expect(html).toContain("Boudicca");
  });

  it("Todo badges readiness: a staged task is Ready, a prepared one is Not ready", () => {
    const html = render(
      makeBoard([
        makeCard({ id: "r", column_id: "opened", kind: "task", issue_id: "iss-1", ready: true, title: "Staged" }),
        makeCard({ id: "d", column_id: "opened", kind: "task", issue_id: "iss-2", title: "Prep" }),
      ]),
    );
    // Badge spans (not the header filter chips, which are buttons).
    expect(html).toContain(">Ready</span>");
    expect(html).toContain(">Not ready</span>");
  });

  it("a queued run is badged Ready (cleared to launch) with its slot position", () => {
    const html = render(
      makeBoard([
        makeCard({
          id: "q",
          column_id: "opened",
          kind: "run",
          run_id: "r1",
          status: "queued",
          queue_position: 3,
          title: "Queued",
        }),
      ]),
    );
    expect(html).toContain(">Ready</span>");
    expect(html).toContain("#3");
  });

  it("renders a failed pipeline in the Closed lane with its reason and Retry", () => {
    const html = render(
      makeBoard([
        makeCard({
          id: "failed",
          column_id: "closed",
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
    expect(html).toContain(">Failed</span>"); // outcome badge (not the filter chip)
    expect(html).toContain("Retry"); // ticket-backed → retryable to Todo
  });

  it("a failed pipeline without a ticket shows the reason but no Retry", () => {
    const html = render(
      makeBoard([
        makeCard({
          id: "failed-standalone",
          column_id: "closed",
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

  it("a successful Closed card shows a Success badge and no error/Retry", () => {
    const html = render(
      makeBoard([
        makeCard({
          id: "ok",
          column_id: "closed",
          kind: "run",
          issue_id: "iss-9",
          run_id: "run-ok",
          status: "finished",
          title: "Clean finish",
        }),
      ]),
    );
    expect(html).toContain(">Success</span>"); // outcome badge
    expect(html).not.toContain(">Failed</span>"); // no failed badge (the Failed filter chip is a button)
    expect(html).not.toContain("Retry"); // a success is not retryable
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
              updated_at: "2026-07-15T09:30:00Z",
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
    // A HEALTHY blocked pipeline shows no redundant status chip (Blocked
    // already says it) and no descendant-count badge.
    expect(html).not.toContain("Running</span>");
    expect(html).not.toContain("Interrupted");
    expect(html).not.toContain("child");
  });

  it("a blocked pipeline shows ONLY the Blocked tag, even when its root is resumable-failed", () => {
    // A resumable process state (restart-orphaned parked parent) is the
    // system's business — the card must not surface it; the sidebar
    // explains it next to the review form.
    const html = render(
      makeBoard([
        makeCard({
          id: "run:orphan",
          column_id: "in_progress",
          title: "Orphaned pipeline",
          run_id: "run-orphan",
          status: "failed_resumable", // e.g. server restart killed the parked parent
          failed: true,
          tree_executed_nodes: 3,
          tree_total_nodes: 10,
          pending_reviews: [
            {
              run_id: "c1",
              node_id: "review",
              depth: 1,
              updated_at: "2026-07-15T09:30:00Z",
            },
          ],
        }),
      ]),
    );
    expect(html).toContain("Blocked — human review · review");
    expect(html).not.toContain("Interrupted");
    expect(html).not.toContain("Failed (resumable)");
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
            {
              run_id: "c1",
              node_id: "review",
              depth: 1,
              updated_at: "2026-07-15T09:30:00Z",
            },
            {
              run_id: "c2",
              node_id: "review",
              depth: 1,
              updated_at: "2026-07-15T09:31:00Z",
            },
            {
              run_id: "c3",
              node_id: "review",
              depth: 1,
              updated_at: "2026-07-15T09:32:00Z",
            },
          ],
        }),
      ]),
    );
    expect(html).toContain("Blocked — 3 human reviews");
    expect(html).toContain("2 / 8 nodes");
  });
});

describe("per-column filters", () => {
  it("renders In progress section then Inventory (Opened tab by default)", () => {
    const html = render(makeBoard([]));
    expect(html).toContain("In progress");
    expect(html).toContain("Inventory");
    expect(html).toContain("Nothing running right now.");
  });
});

describe("parent / child cards are ordinary, ungrouped cards", () => {
  const parent = makeCard({
    id: "p",
    column_id: "closed",
    kind: "run",
    issue_id: "iss-parent",
    run_id: "r-parent",
    status: "finished",
    title: "Boudicca",
    role: "planner",
    children_summary: { total: 5, ready: 0, in_progress: 1, done: 2, failed: 2, open: 0 },
  });
  const kid = makeCard({
    id: "k1",
    column_id: "opened",
    kind: "task",
    issue_id: "k1",
    title: "ÉP 1",
    parent_issue_id: "iss-parent",
    parent_title: "Boudicca",
  });
  const loose = makeCard({
    id: "loose",
    column_id: "opened",
    kind: "task",
    issue_id: "loose",
    title: "Standalone",
  });

  // The parent has children still open, yet its own run finished — it must
  // sit in Closed like any other finished card. No campaign hold, no
  // expand/collapse, no stacked face.
  it("shows every card in one flat grid, with no expand affordance", () => {
    const html = render(makeBoard([parent, kid, loose]));
    expect(html).toContain("Boudicca");
    expect(html).toContain("ÉP 1");
    expect(html).toContain("Standalone");
    expect(html).not.toContain("data-plan-stack");
    expect(html).not.toContain("child tickets of");
    expect(html).not.toContain("Awaiting children");
  });

  // The only surviving link on the child face: the parent name above the
  // title. Purely typographic — the card keeps the standard border, no
  // accent rule singling it out.
  it("names the parent above a child's title", () => {
    const html = render(makeBoard([kid]));
    expect(html).toContain("Spawned by Boudicca");
    expect(html).not.toContain("border-l-accent");
  });

  it("leaves an unrelated card free of the parent tie", () => {
    const html = render(makeBoard([loose]));
    expect(html).not.toContain("Spawned by");
  });
});

describe("children counter on a planner parent", () => {
  // Mirrors the In-progress node bar, but counts CLOSED children (done +
  // failed) over the total — the campaign-progress face of a plan group.
  it("renders 'N / M closed' with a progressbar on the parent card", () => {
    const html = render(
      makeBoard([
        makeCard({
          id: "p",
          column_id: "opened",
          kind: "task",
          issue_id: "parent-1",
          title: "Planner parent",
          role: "planner",
          children_summary: {
            total: 5,
            ready: 0,
            in_progress: 3,
            done: 1,
            failed: 1,
            open: 0,
          },
        }),
      ]),
    );
    expect(html).toContain("2 / 5 closed");
    expect(html).toContain('aria-label="2 of 5 children closed"');
  });

  it("omits the counter when the card has no children", () => {
    const html = render(
      makeBoard([makeCard({ id: "s", column_id: "opened", kind: "task", issue_id: "solo" })]),
    );
    expect(html).not.toContain("children closed");
    expect(html).not.toContain(" closed</div>");
  });
});

describe("priority", () => {
  it("shows the P badge on Todo (draft) and Closed (failed) cards", () => {
    const html = render(
      makeBoard([
        makeCard({ id: "d", column_id: "opened", kind: "task", issue_id: "iss-1", priority: 5 }),
        makeCard({
          id: "f",
          column_id: "closed",
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
    expect(html).toContain("P2");
    expect(html).toContain("launch first once ready");
  });

  it("hides the badge for unprioritized (0) cards", () => {
    const html = render(
      makeBoard([
        makeCard({ id: "d", column_id: "opened", kind: "task", issue_id: "iss-1", priority: 0 }),
      ]),
    );
    expect(html).not.toContain(">P0<");
  });
});

describe("card details affordance", () => {
  it("is clickable with primary Open run when onOpenCard is given", () => {
    const html = renderToStaticMarkup(
      <PipelineColumns
        board={makeBoard([
          makeCard({ id: "a", column_id: "in_progress", run_id: "r1", status: "running", title: "Running" }),
        ])}
        onRefetch={() => {}}
        onOpenCard={() => {}}
      />,
    );
    expect(html).toContain("Open run");
    expect(html).toContain("cursor-pointer");
    expect(html).toContain("More actions for Running");
  });

  it("omits pointer cursor when onOpenCard is absent", () => {
    const html = render(
      makeBoard([
        makeCard({ id: "a", column_id: "in_progress", run_id: "r1", status: "running", title: "Running" }),
      ]),
    );
    expect(html).not.toContain("cursor-pointer");
  });
});

describe("ready toggle buttons (no drag & drop)", () => {
  it("a draft Opened task shows Mark ready; a ready Opened task shows Unmark ready", () => {
    const html = render(
      makeBoard([
        makeCard({ id: "d", column_id: "opened", kind: "task", issue_id: "iss-1", title: "Prep me" }),
        makeCard({ id: "r", column_id: "opened", kind: "task", issue_id: "iss-2", ready: true, title: "Staged" }),
      ]),
    );
    expect(html).toContain("Mark ready");
    expect(html).toContain("Unmark ready");
  });

  it("run-backed cards get no ready-toggle primary actions", () => {
    const html = render(
      makeBoard([
        makeCard({ id: "q", column_id: "opened", kind: "run", run_id: "r1", status: "queued", issue_id: "iss-3" }),
        makeCard({ id: "p", column_id: "in_progress", kind: "run", run_id: "r2", status: "running" }),
        makeCard({ id: "done", column_id: "closed", kind: "run", run_id: "r3", status: "finished" }),
      ]),
    );
    expect(html).not.toContain("Mark ready");
    expect(html).not.toContain("Unmark ready");
  });
});

describe("card actions (delete / pause / resume / reset / stop)", () => {
  it("gates secondary actions via capabilities (menu-only in UI)", () => {
    const draft = makeCard({ column_id: "opened", kind: "task", issue_id: "iss-1" });
    const ready = makeCard({
      column_id: "opened",
      kind: "task",
      issue_id: "iss-2",
      ready: true,
    });
    const live = makeCard({
      column_id: "in_progress",
      kind: "run",
      run_id: "r1",
      issue_id: "iss-3",
      status: "running",
    });
    const paused = makeCard({
      column_id: "in_progress",
      kind: "run",
      run_id: "r1",
      issue_id: "iss-1",
      status: "paused_operator",
    });
    const lone = makeCard({
      column_id: "in_progress",
      kind: "run",
      run_id: "r1",
      status: "running",
    });
    expect(canDeleteTicket(draft)).toBe(true);
    expect(canDeleteTicket(ready)).toBe(false);
    expect(canPauseRun(live)).toBe(true);
    expect(canResetTicket(live)).toBe(true);
    expect(canStopRun(live)).toBe(false);
    expect(canResumeRun(paused)).toBe(true);
    expect(canPauseRun(paused)).toBe(false);
    expect(canStopRun(lone)).toBe(true);
    expect(canResetTicket(lone)).toBe(false);
  });

  it("running card primary is Open run; paused primary is Resume", () => {
    expect(
      render(
        makeBoard([
          makeCard({
            id: "p",
            column_id: "in_progress",
            kind: "run",
            run_id: "r1",
            issue_id: "iss-1",
            status: "running",
            title: "Live",
          }),
        ]),
      ),
    ).toContain("Open run");
    expect(
      render(
        makeBoard([
          makeCard({
            id: "p",
            column_id: "in_progress",
            kind: "run",
            run_id: "r1",
            issue_id: "iss-1",
            status: "paused_operator",
            title: "Parked",
          }),
        ]),
      ),
    ).toContain("Resume");
  });
});

describe("readiness helpers", () => {
  it("cardReady: staged tasks and queued runs are ready; drafts are not", () => {
    expect(cardReady(makeCard({ ready: true }))).toBe(true);
    expect(cardReady(makeCard({ status: "queued" }))).toBe(true);
    expect(cardReady(makeCard({ kind: "task", ready: false }))).toBe(false);
    expect(cardReady(makeCard({ status: "running" }))).toBe(false);
  });

  it("closedOutcome: failed flag splits success from failure", () => {
    expect(closedOutcome(makeCard({ failed: true }))).toBe("failed");
    expect(closedOutcome(makeCard({ status: "finished" }))).toBe("success");
  });
});

describe("action helpers", () => {
  it("canMarkReady: draft Todo tasks stage; failed Closed tickets retry", () => {
    expect(canMarkReady(makeCard({ column_id: "opened", kind: "task", issue_id: "iss-1" }))).toBe(true);
    expect(
      canMarkReady(makeCard({ column_id: "closed", kind: "run", issue_id: "iss-1", failed: true })),
    ).toBe(true);
    // Already-ready Todo tasks, successful Closed cards, ticket-less, or
    // executing cards cannot be marked ready.
    expect(
      canMarkReady(makeCard({ column_id: "opened", kind: "task", issue_id: "iss-1", ready: true })),
    ).toBe(false);
    expect(canMarkReady(makeCard({ column_id: "closed", kind: "run", issue_id: "iss-1" }))).toBe(false);
    expect(canMarkReady(makeCard({ column_id: "opened", kind: "task" }))).toBe(false);
    expect(canMarkReady(makeCard({ column_id: "closed", kind: "run", failed: true }))).toBe(false);
  });

  it("canUnmarkReady: only staged (ready) Todo task cards", () => {
    expect(
      canUnmarkReady(makeCard({ column_id: "opened", kind: "task", issue_id: "iss-1", ready: true })),
    ).toBe(true);
    // A queued RUN in Todo is fixed by run state.
    expect(
      canUnmarkReady(makeCard({ column_id: "opened", kind: "run", issue_id: "iss-1", status: "queued" })),
    ).toBe(false);
    expect(
      canUnmarkReady(makeCard({ column_id: "opened", kind: "task", issue_id: "iss-1" })),
    ).toBe(false); // draft, not ready
    expect(canUnmarkReady(makeCard({ column_id: "opened", kind: "task", ready: true }))).toBe(false); // no ticket
  });

  it("canDeleteTicket: only draft (not-ready) Todo task cards", () => {
    expect(canDeleteTicket(makeCard({ column_id: "opened", kind: "task", issue_id: "iss-1" }))).toBe(true);
    expect(
      canDeleteTicket(makeCard({ column_id: "opened", kind: "task", issue_id: "iss-1", ready: true })),
    ).toBe(false); // ready → unmark first
    expect(
      canDeleteTicket(makeCard({ column_id: "opened", kind: "run", issue_id: "iss-1", status: "queued" })),
    ).toBe(false);
    expect(canDeleteTicket(makeCard({ column_id: "opened", kind: "task" }))).toBe(false);
    expect(canDeleteTicket(makeCard({ column_id: "closed", kind: "run", issue_id: "iss-1", failed: true }))).toBe(
      false,
    );
  });

  it("canPauseRun / canResumeRun: gated on the exact live status", () => {
    expect(
      canPauseRun(makeCard({ column_id: "in_progress", run_id: "r1", status: "running" })),
    ).toBe(true);
    expect(
      canPauseRun(makeCard({ column_id: "in_progress", run_id: "r1", status: "paused_waiting_human" })),
    ).toBe(false);
    expect(canPauseRun(makeCard({ column_id: "in_progress", status: "running" }))).toBe(false);
    expect(
      canResumeRun(makeCard({ column_id: "in_progress", run_id: "r1", status: "paused_operator" })),
    ).toBe(true);
    expect(
      canResumeRun(makeCard({ column_id: "in_progress", run_id: "r1", status: "running" })),
    ).toBe(false);
  });

  it("canResetTicket / canStopRun: ticket-backed resets, standalone stops", () => {
    expect(
      canResetTicket(
        makeCard({ column_id: "in_progress", run_id: "r1", issue_id: "iss-1", status: "running" }),
      ),
    ).toBe(true);
    expect(
      canResetTicket(makeCard({ column_id: "in_progress", run_id: "r1", status: "running" })),
    ).toBe(false);
    expect(
      canResetTicket(makeCard({ column_id: "opened", kind: "task", issue_id: "iss-1" })),
    ).toBe(false);
    expect(
      canStopRun(makeCard({ column_id: "in_progress", run_id: "r1", status: "running" })),
    ).toBe(true);
    expect(
      canStopRun(
        makeCard({ column_id: "in_progress", run_id: "r1", issue_id: "iss-1", status: "running" }),
      ),
    ).toBe(false);
    expect(canStopRun(makeCard({ column_id: "closed", run_id: "r1", status: "finished" }))).toBe(false);
  });

  it("moveTicket(true) stages ready, then refetches", async () => {
    markReadyMock.mockResolvedValue(undefined);
    const onDone = vi.fn();
    await moveTicket("iss-1", true, onDone);
    expect(markReadyMock).toHaveBeenCalledWith("iss-1", true);
    expect(onDone).toHaveBeenCalledTimes(1);
  });

  it("moveTicket(false) unstages back to draft", async () => {
    markReadyMock.mockResolvedValue(undefined);
    const onDone = vi.fn();
    await moveTicket("iss-1", false, onDone);
    expect(markReadyMock).toHaveBeenCalledWith("iss-1", false);
    expect(onDone).toHaveBeenCalledTimes(1);
  });
});

describe("isTicketEditable", () => {
  it("allows editing not-yet-run Todo tickets and failed Closed ones", () => {
    expect(
      isTicketEditable(makeCard({ column_id: "opened", kind: "task", issue_id: "iss-1" })),
    ).toBe(true);
    expect(
      isTicketEditable(makeCard({ column_id: "opened", kind: "task", issue_id: "iss-2", ready: true })),
    ).toBe(true);
    expect(
      isTicketEditable(
        makeCard({ column_id: "closed", kind: "run", issue_id: "iss-3", failed: true }),
      ),
    ).toBe(true);
  });

  it("blocks editing executing / successful / queued cards", () => {
    expect(
      isTicketEditable(
        makeCard({ column_id: "in_progress", kind: "run", issue_id: "iss-4", status: "running" }),
      ),
    ).toBe(false);
    expect(
      isTicketEditable(
        makeCard({ column_id: "closed", kind: "run", issue_id: "iss-5", status: "finished" }),
      ),
    ).toBe(false); // successful → history, not editable
    expect(
      isTicketEditable(
        makeCard({ column_id: "opened", kind: "run", issue_id: "iss-6", status: "queued" }),
      ),
    ).toBe(false);
    expect(isTicketEditable(makeCard({ column_id: "opened", kind: "task" }))).toBe(false);
  });

  it("marks opened tasks editable; running cards are not", () => {
    expect(
      isTicketEditable(
        makeCard({ column_id: "opened", kind: "task", issue_id: "iss-1", title: "Draft task" }),
      ),
    ).toBe(true);
    expect(
      isTicketEditable(
        makeCard({ column_id: "in_progress", kind: "run", run_id: "run-x", status: "running" }),
      ),
    ).toBe(false);
  });
});

// Drag & drop exists for exactly ONE transition: Opened → In progress, the
// operator's override of the priority queue. canLaunchNow mirrors the
// server's guards so the UI never offers a drag the backend would refuse.
describe("launch-now drag (Opened → In progress)", () => {
  const draggable = makeCard({
    id: "t",
    column_id: "opened",
    kind: "task",
    issue_id: "iss-1",
    title: "Draft",
  });

  it("marks an eligible Opened ticket as draggable", () => {
    const html = render(makeBoard([draggable]));
    expect(html).toContain('draggable="true"');
    expect(html).toContain("Drag onto In progress to start now");
    expect(html).toContain("data-launch-dropzone");
  });

  it("allows a plain Opened ticket", () => {
    expect(canLaunchNow(draggable)).toBe(true);
  });

  it("refuses cards the server would 409 on", () => {
    // Already launched — Reset/Retry own that transition, not the drag.
    expect(
      canLaunchNow(makeCard({ ...draggable, run_id: "r1", status: "queued" })),
    ).toBe(false);
    // Unfinished hard dependency: correctness, not ranking.
    expect(canLaunchNow(makeCard({ ...draggable, open_blocker_count: 2 }))).toBe(
      false,
    );
    // Already closed, or a run card with no ticket to launch.
    expect(canLaunchNow(makeCard({ ...draggable, column_id: "closed" }))).toBe(
      false,
    );
    expect(canLaunchNow(makeCard({ ...draggable, kind: "run" }))).toBe(false);
    expect(canLaunchNow(makeCard({ ...draggable, issue_id: undefined }))).toBe(
      false,
    );
  });

  it("does not make Closed cards draggable", () => {
    const html = render(
      makeBoard([
        makeCard({
          id: "c",
          column_id: "closed",
          kind: "run",
          issue_id: "iss-2",
          run_id: "r2",
          status: "finished",
          title: "Done",
        }),
      ]),
    );
    expect(html).not.toContain('draggable="true"');
  });

  it("uses a bespoke MIME type so stray drags are ignored", () => {
    expect(LAUNCH_DRAG_TYPE).toBe("application/x-iterion-ticket");
  });
});

// "Edit bot" opens the BOT's main.bot in the editor — distinct from "Edit",
// which edits the ticket. The path comes from the catalog's rel_path, the
// same derivation the Catalog dialog uses.
describe("botEditorPath", () => {
  const catalog = [
    { name: "shorts-episode", rel_path: "bots/shorts-episode", is_bundle: true },
    { name: "loose", path: "/abs/loose.bot", is_bundle: false },
    { name: "no-rel", is_bundle: true },
  ];

  it("resolves a bundle to its main.bot", () => {
    expect(botEditorPath("shorts-episode", catalog)).toBe(
      "bots/shorts-episode/main.bot",
    );
  });

  it("returns null when there is nothing stable to open", () => {
    // A loose .bot is not an editable bundle; a bundle the server could not
    // relativise has no workspace path; an unknown or absent bot_id has no
    // entry at all. In every case the menu entry is withheld rather than
    // pointing at a file the editor would fail to open.
    expect(botEditorPath("loose", catalog)).toBeNull();
    expect(botEditorPath("no-rel", catalog)).toBeNull();
    expect(botEditorPath("ghost", catalog)).toBeNull();
    expect(botEditorPath(undefined, catalog)).toBeNull();
    expect(botEditorPath("  ", catalog)).toBeNull();
    // Catalog not loaded yet.
    expect(botEditorPath("shorts-episode", null)).toBeNull();
  });
});

describe("Edit bot menu entry", () => {
  const withBot = makeCard({
    id: "d",
    column_id: "opened",
    kind: "task",
    issue_id: "iss-1",
    title: "Draft",
    bot_id: "shorts-episode",
  });
  const kinds = (card: PipelineBoardCard) =>
    resolveMenuItems(card, resolvePrimaryAction(card).kind).map((i) => i.kind);

  // The ⋯ menu is a Radix dropdown: its items only exist in the DOM once
  // opened, so the contract is asserted on the resolver, not the markup.
  it("is offered on an Opened card that names a bot", () => {
    expect(kinds(withBot)).toContain("edit_bot");
  });

  it("is distinct from Edit, which edits the ticket", () => {
    const items = resolveMenuItems(withBot, resolvePrimaryAction(withBot).kind);
    const editBot = items.find((i) => i.kind === "edit_bot");
    expect(editBot?.label).toBe("Edit bot");
    expect(items.find((i) => i.kind === "edit")?.label).toBe("Edit");
  });

  it("is withheld when the card names no bot", () => {
    expect(kinds(makeCard({ ...withBot, bot_id: undefined }))).not.toContain(
      "edit_bot",
    );
  });
});
