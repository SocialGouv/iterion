import { afterEach, describe, expect, it, vi } from "vitest";

import {
  createPipelineTask,
  getPipelineBoard,
  normalizePipelineBoard,
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
  it("keeps the four fixed columns, folded root cards and concurrency", () => {
    const board = normalizePipelineBoard({
      columns: [
        { id: "todo", title: "Todo", kind: "todo" },
        { id: "in_progress", title: "In progress", kind: "in_progress" },
        { id: "done", title: "Done", kind: "done" },
        { id: "attention", title: "Attention", kind: "attention" },
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
              questions: { approved: "Ship it?" },
            },
          ],
          created_at: "2026-07-14T09:00:00Z",
          updated_at: "2026-07-14T10:00:00Z",
        },
        {
          id: "task:iss-1",
          kind: "task",
          column_id: "todo",
          title: "Backlog item",
          issue_id: "iss-1",
          issue_state: "ready",
          entry_input: { area: "api", verbose: true },
          queue_position: 2,
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
      "todo",
      "in_progress",
      "done",
      "attention",
    ]);
    expect(board.concurrency).toEqual({
      enabled: true,
      max: 3,
      active: 2,
      waiting: 1,
    });
    expect(board.cards[0]).toMatchObject({
      run_id: "run-root",
      tree_executed_nodes: 12,
      tree_total_nodes: 40,
      descendant_count: 2,
    });
    expect(board.cards[0]?.pending_reviews).toHaveLength(1);
    expect(board.cards[0]?.pending_reviews?.[0]).toMatchObject({
      run_id: "run-child",
      node_id: "approval",
      depth: 1,
      questions: { approved: "Ship it?" },
    });
    expect(board.cards[1]).toMatchObject({
      kind: "task",
      column_id: "todo",
      entry_input: { area: "api", verbose: true },
      queue_position: 2,
    });
  });

  it("defaults progress and concurrency when the server omits them", () => {
    const board = normalizePipelineBoard({
      cards: [{ id: "task:x", column_id: "todo", title: "Loose" }],
    });
    expect(board.columns).toEqual([]);
    expect(board.concurrency).toEqual({
      enabled: false,
      max: 0,
      active: 0,
      waiting: 0,
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
});

describe("getPipelineBoard", () => {
  it("GETs the single global board endpoint", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      jsonResponse({
        columns: [{ id: "todo", title: "Todo", kind: "todo" }],
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
