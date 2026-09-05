import { afterEach, describe, expect, it, vi } from "vitest";

import {
  createPipelineTask,
  getPipelineBoard,
  markPipelineTaskReady,
  normalizePipelineBoard,
  updatePipelineTask,
} from "./pipelineBoards";

afterEach(() => {
  vi.unstubAllGlobals();
});

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

describe("normalizePipelineBoard", () => {
  it("keeps the three fixed columns, folded root cards, ready flags and concurrency", () => {
    const board = normalizePipelineBoard({
      columns: [
        { id: "opened", title: "Opened", kind: "opened" },
        { id: "in_progress", title: "In progress", kind: "in_progress" },
        {
          id: "needs_attention",
          title: "Needs attention",
          kind: "needs_attention",
        },
        { id: "closed", title: "Closed", kind: "closed" },
      ],
      cards: [
        {
          id: "run:run-root",
          kind: "run",
          column_id: "in_progress",
          title: "Ship the feature",
          run_id: "run-root",
          bot_id: "feature-dev",
          status: "running",
          executed_nodes: 3,
          total_nodes: 8,
          tree_executed_nodes: 12,
          tree_total_nodes: 40,
          descendant_count: 2,
          pending_reviews: [
            {
              run_id: "run-child",
              node_id: "approval",
              depth: 1,
              updated_at: "2026-07-14T09:30:00Z",
              questions: { approved: "Ship it?" },
            },
          ],
          created_at: "2026-07-14T09:00:00Z",
          updated_at: "2026-07-14T10:00:00Z",
        },
        {
          id: "task:iss-1",
          kind: "task",
          column_id: "opened",
          title: "Backlog item",
          issue_id: "iss-1",
          issue_state: "ready",
          ready: true,
          entry_input: { area: "api", verbose: true },
          queue_position: 2,
          executed_nodes: 0,
          total_nodes: 0,
          tree_executed_nodes: 0,
          tree_total_nodes: 0,
          created_at: "2026-07-14T09:00:00Z",
          updated_at: "2026-07-14T09:00:00Z",
        },
        {
          id: "task:iss-2",
          kind: "run",
          column_id: "needs_attention",
          title: "Broke last time",
          issue_id: "iss-2",
          run_id: "run-old",
          failed: true,
          reserves_slot: true,
          error: "boom",
          executed_nodes: 0,
          total_nodes: 0,
          tree_executed_nodes: 0,
          tree_total_nodes: 0,
          created_at: "2026-07-14T09:00:00Z",
          updated_at: "2026-07-14T09:00:00Z",
        },
      ],
      concurrency: { enabled: true, max: 3, active: 2, waiting: 1 },
      generated_at: "2026-07-14T10:00:00Z",
    });

    expect(board.columns.map((c) => c.id)).toEqual([
      "opened",
      "in_progress",
      "needs_attention",
      "closed",
    ]);
    expect(board.concurrency).toEqual({
      enabled: true,
      max: 3,
      active: 2,
      waiting: 1,
      // Absent from this fixture (an older server) → 0, so the chip's
      // arithmetic degrades to exactly today's behaviour.
      reserved: 0,
    });
    expect(board.cards[0]).toMatchObject({
      run_id: "run-root",
      tree_executed_nodes: 12,
      tree_total_nodes: 40,
      descendant_count: 2,
    });
    // The normalizer is a WHITELIST — a field it does not list never reaches
    // the views however faithfully the server emits it. reserves_slot drives
    // the "Holds a slot" badge, so losing it here would silently strip the
    // only on-card explanation for why the board stopped launching.
    const attention = board.cards.find((c) => c.column_id === "needs_attention");
    expect(attention?.reserves_slot).toBe(true);
    expect(board.cards[0]?.pending_reviews).toHaveLength(1);
    expect(board.cards[0]?.pending_reviews?.[0]).toMatchObject({
      run_id: "run-child",
      node_id: "approval",
      depth: 1,
      updated_at: "2026-07-14T09:30:00Z",
      questions: { approved: "Ship it?" },
    });
    expect(board.cards[1]).toMatchObject({
      kind: "task",
      column_id: "opened",
      ready: true,
      entry_input: { area: "api", verbose: true },
      queue_position: 2,
    });
    expect(board.cards[2]).toMatchObject({
      column_id: "needs_attention",
      failed: true,
      error: "boom",
    });
  });

  it("defaults progress and concurrency when the server omits them", () => {
    const board = normalizePipelineBoard({
      cards: [{ id: "task:x", column_id: "opened", title: "Loose" }],
    });
    expect(board.columns).toEqual([]);
    expect(board.concurrency).toEqual({
      enabled: false,
      max: 0,
      active: 0,
      waiting: 0,
      reserved: 0,
    });
    expect(board.cards[0]).toMatchObject({
      kind: "task",
      executed_nodes: 0,
      total_nodes: 0,
      tree_executed_nodes: 0,
      tree_total_nodes: 0,
    });
    expect(board.cards[0]?.pending_reviews).toBeUndefined();
  });

  // Regression: this normalizer is a whitelist, and planner provenance was
  // missing from it — so a parent card arrived in the views with no
  // children_summary and rendered neither the Plan badge nor the
  // "N / M closed" children counter, even though the server sent both.
  it("keeps planner provenance (role, parent, children, summary)", () => {
    const board = normalizePipelineBoard({
      cards: [
        {
          id: "run:parent",
          column_id: "opened",
          title: "Boudicca",
          run_id: "run-1",
          role: "planner",
          children: [
            { issue_id: "kid-1", title: "ÉP 1", state: "done", card_id: "run:kid1" },
            { issue_id: "", title: "dropped — no issue_id" },
          ],
          children_summary: {
            total: 5,
            ready: 0,
            in_progress: 1,
            done: 2,
            failed: 2,
            open: 0,
          },
        },
        {
          id: "run:child",
          column_id: "in_progress",
          title: "ÉP 4/5",
          parent_issue_id: "native:parent",
          parent_title: "Boudicca",
          role: "producer",
        },
      ],
    });
    const parent = board.cards[0];
    expect(parent?.role).toBe("planner");
    expect(parent?.children_summary).toEqual({
      total: 5,
      ready: 0,
      in_progress: 1,
      done: 2,
      failed: 2,
      open: 0,
    });
    expect(parent?.children).toEqual([
      { issue_id: "kid-1", title: "ÉP 1", state: "done", card_id: "run:kid1" },
    ]);
    const child = board.cards[1];
    expect(child?.parent_issue_id).toBe("native:parent");
    expect(child?.parent_title).toBe("Boudicca");
    expect(child?.role).toBe("producer");
  });

  it("keeps a give-up stamp, and drops one that names no run", () => {
    const board = normalizePipelineBoard({
      cards: [
        {
          id: "run:a",
          column_id: "needs_attention",
          title: "Given up on",
          gave_up: {
            run_id: "run-dead",
            state: "blocked",
            attempts: 3,
            at: "2026-08-23T09:00:00Z",
          },
        },
        {
          // A stamp with no run cannot be attributed to the card showing it,
          // so rendering a claim about an unknown run would be worse than
          // rendering nothing.
          id: "run:b",
          column_id: "needs_attention",
          title: "Unattributable",
          gave_up: { state: "blocked", attempts: 2 },
        },
        {
          // The watchdog's own verdict carries a reason instead of an
          // attempt count — it must survive normalisation, it is what the
          // operator reads.
          id: "run:c",
          column_id: "needs_attention",
          title: "Pruned pointer",
          gave_up: {
            run_id: "run-pruned",
            state: "blocked",
            reason: "recorded run run-pruned is gone (pruned or deleted)",
          },
        },
      ],
    });
    expect(board.cards[0]?.gave_up).toEqual({
      run_id: "run-dead",
      state: "blocked",
      attempts: 3,
      at: "2026-08-23T09:00:00Z",
    });
    expect(board.cards[1]?.gave_up).toBeUndefined();
    expect(board.cards[2]?.gave_up).toEqual({
      run_id: "run-pruned",
      state: "blocked",
      reason: "recorded run run-pruned is gone (pruned or deleted)",
    });
  });

  it("omits planner provenance when the server sends none", () => {
    const board = normalizePipelineBoard({
      cards: [{ id: "task:x", column_id: "opened", title: "Loose" }],
    });
    expect(board.cards[0]?.children_summary).toBeUndefined();
    expect(board.cards[0]?.children).toBeUndefined();
    expect(board.cards[0]?.role).toBeUndefined();
  });
});

describe("getPipelineBoard", () => {
  it("GETs the single global board endpoint", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse({
        columns: [{ id: "opened", title: "Opened", kind: "opened" }],
        cards: [],
        concurrency: { enabled: false, max: 0, active: 0, waiting: 0 },
        generated_at: "2026-07-14T10:00:00Z",
      }),
    );
    vi.stubGlobal("fetch", fetchMock);

    const board = await getPipelineBoard();

    expect(board.columns).toHaveLength(1);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [path] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(path).toBe("/api/v1/pipeline-board");
  });
});

describe("createPipelineTask", () => {
  it("POSTs to the global tasks endpoint with the bot in the body", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse(
        {
          id: "issue-1",
          state: "ready",
          title: "Ship docs",
          bot: "docs/review",
          created_at: "2026-07-14T10:00:00Z",
          updated_at: "2026-07-14T10:00:00Z",
        },
        201,
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    const issue = await createPipelineTask({
      bot: "docs/review",
      title: "Ship docs",
      labels: ["docs"],
      priority: 2,
      bot_args: { area: "api" },
      start: true,
    });

    expect(issue).toMatchObject({ id: "issue-1", state: "ready", bot: "docs/review" });
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [path, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(path).toBe("/api/v1/pipeline-board/tasks");
    expect(init.method).toBe("POST");
    expect(JSON.parse(String(init.body))).toEqual({
      bot: "docs/review",
      title: "Ship docs",
      labels: ["docs"],
      priority: 2,
      bot_args: { area: "api" },
      start: true,
    });
  });
});

describe("markPipelineTaskReady", () => {
  it("POSTs the ready flag to the task's ready endpoint", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);

    await markPipelineTaskReady("iss 1/a", true);

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [path, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(path).toBe("/api/v1/pipeline-board/tasks/iss%201%2Fa/ready");
    expect(init.method).toBe("POST");
    expect(JSON.parse(String(init.body))).toEqual({ ready: true });
  });

  it("sends ready:false when unmarking", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);

    await markPipelineTaskReady("iss-9", false);

    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(JSON.parse(String(init.body))).toEqual({ ready: false });
  });
});

describe("updatePipelineTask", () => {
  it("PATCHes the ticket's endpoint with the patch body", async () => {
    const fetchMock = vi
      .fn()
      .mockResolvedValue(new Response(null, { status: 204 }));
    vi.stubGlobal("fetch", fetchMock);

    await updatePipelineTask("iss 1/a", {
      title: "New title",
      body: "context",
      labels: ["docs"],
      priority: 3,
      bot: "docs/review",
      bot_args: { area: "api" },
    });

    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [path, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(path).toBe("/api/v1/pipeline-board/tasks/iss%201%2Fa");
    expect(init.method).toBe("PATCH");
    expect(JSON.parse(String(init.body))).toEqual({
      title: "New title",
      body: "context",
      labels: ["docs"],
      priority: 3,
      bot: "docs/review",
      bot_args: { area: "api" },
    });
  });
});
