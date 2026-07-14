import { afterEach, describe, expect, it, vi } from "vitest";

import {
  createPipelineTask,
  normalizePipelineBoardDetail,
  normalizePipelineBoardList,
} from "./pipelineBoards";

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("pipelineBoards normalizers", () => {
  it("keeps the canonical board, topology and flat run-tree cards", () => {
    const detail = normalizePipelineBoardDetail({
      board: {
        id: "feature-dev",
        bot_id: "feature-dev",
        display_name: "Featurly",
        enabled: true,
      },
      columns: [
        { id: "todo", title: "Todo", kind: "todo" },
        {
          id: "approval",
          title: "Approval",
          kind: "interaction",
          node_id: "approval",
        },
      ],
      cards: [
        {
          id: "run-child",
          kind: "run",
          column_id: "approval",
          title: "Review shard",
          issue_id: "iss-1",
          run_id: "run-child",
          root_run_id: "run-root",
          parent_run_id: "run-root",
          depth: 1,
          status: "paused_waiting_human",
          node_id: "approval",
          interaction_id: "run-child_approval",
          questions: { summary: "Ready to merge?" },
          children_count: 2,
          attempts: [
            { run_id: "run-old", status: "failed", at: "2026-07-14T08:00:00Z" },
            { run_id: "run-child", status: "paused_waiting_human" },
          ],
        },
      ],
      generated_at: "2026-07-14T10:00:00Z",
    });

    expect(detail.board).toMatchObject({
      id: "feature-dev",
      bot_id: "feature-dev",
      display_name: "Featurly",
      enabled: true,
    });
    expect(detail.columns[1]).toMatchObject({
      id: "approval",
      node_id: "approval",
    });
    expect(detail.cards[0]).toMatchObject({
      run_id: "run-child",
      parent_run_id: "run-root",
      depth: 1,
      questions: { summary: "Ready to merge?" },
      children_count: 2,
    });
    expect(detail.cards[0]?.attempts).toHaveLength(2);
  });

  it("absorbs early aliases, checkpoint questions and numeric attempt counts", () => {
    const detail = normalizePipelineBoardDetail({
      identity: { id: "docs", bot: "docs", name: "Docs", enabled: false },
      columns: [{ node_id: "choose", display_name: "Choose target" }],
      tasks: [
        {
          issue: "iss-2",
          run: "run-2",
          column: "choose",
          workflow_name: "docs_refresh",
          parent_id: "run-root",
          depth: -3,
          checkpoint: {
            node_id: "choose",
            interaction_id: "int-2",
            interaction_questions: { target: "api" },
          },
          attempts: 3,
        },
      ],
    });

    expect(detail.board).toEqual({
      id: "docs",
      bot_id: "docs",
      display_name: "Docs",
      enabled: false,
    });
    expect(detail.cards[0]).toMatchObject({
      issue_id: "iss-2",
      run_id: "run-2",
      parent_run_id: "run-root",
      node_id: "choose",
      interaction_id: "int-2",
      questions: { target: "api" },
      depth: 0,
    });
    expect(detail.cards[0]?.attempts).toHaveLength(3);
  });

  it("normalizes list envelopes and derives counts from detailed entries", () => {
    const list = normalizePipelineBoardList({
      pipeline_boards: [
        {
          board: {
            id: "review",
            bot_id: "review",
            display_name: "Revi",
            enabled: true,
          },
          columns: [{ id: "todo", title: "Todo", kind: "todo" }],
          cards: [
            {
              id: "r1",
              column_id: "todo",
              title: "One",
              status: "paused_waiting_human",
            },
          ],
        },
      ],
    });

    expect(list).toHaveLength(1);
    expect(list[0]).toMatchObject({
      column_count: 1,
      card_count: 1,
      awaiting_input_count: 1,
    });
  });

  it("does not invent zero counts for identity-only list entries", () => {
    const list = normalizePipelineBoardList({
      boards: [
        {
          id: "feature-dev",
          bot_id: "feature-dev",
          display_name: "Featurly",
          enabled: true,
        },
      ],
    });

    expect(list[0]?.board.bot_id).toBe("feature-dev");
    expect(list[0]?.column_count).toBeUndefined();
    expect(list[0]?.card_count).toBeUndefined();
    expect(list[0]?.awaiting_input_count).toBeUndefined();
  });

  it("posts a task to the encoded bot board with the canonical body", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        JSON.stringify({
          id: "issue-1",
          state: "ready",
          title: "Ship docs",
          bot: "docs/review",
          created_at: "2026-07-14T10:00:00Z",
          updated_at: "2026-07-14T10:00:00Z",
        }),
        { status: 201, headers: { "Content-Type": "application/json" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    const issue = await createPipelineTask("docs/review", {
      title: "Ship docs",
      labels: ["docs"],
      priority: 2,
      bot_args: { area: "api" },
      start: true,
    });

    expect(issue).toMatchObject({
      id: "issue-1",
      state: "ready",
      bot: "docs/review",
    });
    expect(fetchMock).toHaveBeenCalledTimes(1);
    const [path, init] = fetchMock.mock.calls[0] as [string, RequestInit];
    expect(path).toBe("/api/v1/pipeline-boards/docs%2Freview/tasks");
    expect(init.method).toBe("POST");
    expect(JSON.parse(String(init.body))).toEqual({
      title: "Ship docs",
      labels: ["docs"],
      priority: 2,
      bot_args: { area: "api" },
      start: true,
    });
  });
});
