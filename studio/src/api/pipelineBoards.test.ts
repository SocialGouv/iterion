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
              media: [
                {
                  run_id: "run-child",
                  path: "renders/final cut.mp4",
                  kind: "video",
                  mime: "video/mp4",
                  size: 4096,
                  caption: "Validate motion and timing",
                },
                { path: "missing-kind.png" },
                { path: "active.svg", kind: "document" },
              ],
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
          column_id: "closed",
          title: "Broke last time",
          issue_id: "iss-2",
          run_id: "run-old",
          failed: true,
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
      "closed",
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
      updated_at: "2026-07-14T09:30:00Z",
      questions: { approved: "Ship it?" },
      media: [
        {
          run_id: "run-child",
          path: "renders/final cut.mp4",
          kind: "video",
          mime: "video/mp4",
          size: 4096,
          caption: "Validate motion and timing",
        },
      ],
    });
    expect(board.cards[1]).toMatchObject({
      kind: "task",
      column_id: "opened",
      ready: true,
      entry_input: { area: "api", verbose: true },
      queue_position: 2,
    });
    expect(board.cards[2]).toMatchObject({
      column_id: "closed",
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

  it("normalizes review media defensively", () => {
    const board = normalizePipelineBoard({
      cards: [
        {
          id: "run:media",
          column_id: "in_progress",
          title: "Media review",
          pending_reviews: [
            {
              run_id: "run-media",
              updated_at: "2026-07-14T09:30:00Z",
              depth: 0,
              media: [
                { path: "cover.png", kind: "image", size: -1 },
                { path: "track.wav", kind: "audio", size: "large" },
                { path: "", kind: "video" },
                { path: "clip.mp4", kind: "html" },
                "not-an-object",
              ],
            },
          ],
        },
      ],
    });

    expect(board.cards[0]?.pending_reviews?.[0]?.media).toEqual([
      { path: "cover.png", kind: "image" },
      { path: "track.wav", kind: "audio" },
    ]);
  });

  it("accepts only a versioned AI review brief containing one to three points", () => {
    const board = normalizePipelineBoard({
      cards: [
        {
          id: "run:briefs",
          column_id: "in_progress",
          title: "Brief validation",
          pending_reviews: [
            {
              run_id: "valid",
              updated_at: "2026-07-14T09:30:00Z",
              depth: 0,
              review_brief: {
                version: 1,
                source: "ai",
                points: ["  Inspect the concept.  ", "Confirm the playable result."],
              },
            },
            {
              run_id: "wrong-source",
              updated_at: "2026-07-14T09:31:00Z",
              depth: 0,
              review_brief: {
                version: 1,
                source: "workflow",
                points: ["Do not trust this."],
              },
            },
            {
              run_id: "too-many",
              updated_at: "2026-07-14T09:32:00Z",
              depth: 0,
              review_brief: {
                version: 1,
                source: "ai",
                points: ["One", "Two", "Three", "Four"],
              },
            },
            {
              run_id: "empty",
              updated_at: "2026-07-14T09:33:00Z",
              depth: 0,
              review_brief: {
                version: 1,
                source: "ai",
                points: ["   "],
              },
            },
          ],
        },
      ],
    });

    const reviews = board.cards[0]?.pending_reviews ?? [];
    expect(reviews[0]?.review_brief).toEqual({
      version: 1,
      source: "ai",
      points: ["Inspect the concept.", "Confirm the playable result."],
    });
    expect(reviews.slice(1).every((review) => review.review_brief === undefined)).toBe(
      true,
    );
  });

  it("normalizes guided review metadata for the existing review flow", () => {
    const board = normalizePipelineBoard({
      cards: [
        {
          id: "run:guided",
          column_id: "in_progress",
          title: "Guided review",
          pending_reviews: [
            {
              run_id: "run-guided",
              updated_at: "2026-07-14T09:30:00Z",
              depth: 0,
              review: {
                turns: [
                  { role: "companion", content: "Play the clip.", verdict: { decision: "approved" } },
                  { role: "invalid", content: "drop me" },
                  "not-an-object",
                ],
                posture: "agent_verdict_ok",
                merge_strategy: "merge",
                merge_into: "main",
                max_turns: 6,
                review_url: "https://review.example.test/clip",
                verdict: { decision: "approved", confidence: "high" },
              },
            },
          ],
        },
      ],
    });

    expect(board.cards[0]?.pending_reviews?.[0]?.review).toEqual({
      turns: [
        {
          role: "companion",
          content: "Play the clip.",
          verdict: { decision: "approved" },
        },
      ],
      posture: "agent_verdict_ok",
      mergeStrategy: "merge",
      mergeInto: "main",
      maxTurns: 6,
      reviewUrl: "https://review.example.test/clip",
      verdict: { decision: "approved", confidence: "high" },
    });
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
