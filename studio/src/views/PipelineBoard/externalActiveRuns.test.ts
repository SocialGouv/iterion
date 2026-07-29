import { describe, expect, it } from "vitest";

import type { GlobalActiveRun } from "@/api/runs";
import type { PipelineBoardCard } from "@/api/pipelineBoards";

import { crossStoreRunHref, externalActiveRuns } from "./externalActiveRuns";

function run(
  id: string,
  workflowName: string,
  updatedAt: string,
  workspaceDir = "/home/user/Workspace/game/town",
): GlobalActiveRun {
  return {
    id,
    workflow_name: workflowName,
    status: "running",
    created_at: updatedAt,
    updated_at: updatedAt,
    store_path: `/tmp/store-${id}`,
    workspace_dir: workspaceDir,
  };
}

function card(
  runID: string,
  workflowName: string,
  treeRunIDs: string[] = [],
): PipelineBoardCard {
  return {
    id: `run:${runID}`,
    kind: "run",
    column_id: "in_progress",
    title: workflowName,
    run_id: runID,
    workflow_name: workflowName,
    tree_run_ids: treeRunIDs,
    executed_nodes: 0,
    total_nodes: 0,
    tree_executed_nodes: 0,
    tree_total_nodes: 0,
    created_at: "2026-07-28T00:00:00Z",
    updated_at: "2026-07-28T00:00:00Z",
  };
}

describe("externalActiveRuns", () => {
  it("removes roots and descendants already represented on the board", () => {
    const cards = [card("local-root", "town_planner", ["local-root", "local-child"])];
    const runs = [
      run("local-root", "town_planner", "2026-07-28T10:00:00Z"),
      run("local-child", "planner_child", "2026-07-28T10:01:00Z"),
      run("external", "town_planner", "2026-07-28T10:02:00Z"),
    ];

    expect(
      externalActiveRuns(runs, cards, "/home/user/Workspace/game/town").map(
        (item) => item.id,
      ),
    ).toEqual(["external"]);
  });

  it("prioritizes alternate attempts of workflows known to this board", () => {
    const cards = [card("old-planner", "town_planner")];
    const runs = [
      run("unrelated-new", "other_bot", "2026-07-28T12:00:00Z"),
      run("active-planner", "town_planner", "2026-07-28T11:00:00Z"),
    ];

    expect(
      externalActiveRuns(runs, cards, "/home/user/Workspace/game/town").map(
        (item) => item.id,
      ),
    ).toEqual(["active-planner", "unrelated-new"]);
  });

  it("uses an explicit live-status allowlist", () => {
    const failed = run("failed", "town_planner", "2026-07-28T12:00:00Z");
    failed.status = "failed_resumable";
    const finished = run(
      "finished",
      "town_planner",
      "2026-07-28T12:01:00Z",
    );
    finished.status = "finished";

    expect(
      externalActiveRuns(
        [failed, finished],
        [],
        "/home/user/Workspace/game/town",
      ),
    ).toEqual([]);
  });

  it("folds external child runs into their root instead of showing fake pipelines", () => {
    const root = run("root", "town_planner", "2026-07-28T12:00:00Z");
    const child = run("child", "scope_survey", "2026-07-28T12:01:00Z");
    child.parent_run_id = root.id;

    expect(
      externalActiveRuns(
        [root, child],
        [],
        "/home/user/Workspace/game/town",
      ).map((item) => item.id),
    ).toEqual(["root"]);
  });

  it("keeps only runs from the current project with a path boundary", () => {
    const project = "/home/user/Workspace/game/town";
    const runs = [
      run("exact", "town_planner", "2026-07-28T12:00:00Z", project),
      run(
        "nested",
        "town_planner",
        "2026-07-28T11:00:00Z",
        `${project}/iterion/bots/town-planner`,
      ),
      run(
        "prefix-only",
        "town_planner",
        "2026-07-28T10:00:00Z",
        `${project}ship`,
      ),
      run(
        "other",
        "town_planner",
        "2026-07-28T09:00:00Z",
        "/home/user/Workspace/video/shorts",
      ),
    ];

    expect(externalActiveRuns(runs, [], project).map((item) => item.id)).toEqual([
      "exact",
      "nested",
    ]);
  });

  it("waits for project identity instead of flashing every global run", () => {
    expect(
      externalActiveRuns(
        [run("external", "town_planner", "2026-07-28T12:00:00Z")],
        [],
        null,
      ),
    ).toEqual([]);
  });

  it("matches nested Windows workspace paths", () => {
    const item = run(
      "windows",
      "town_planner",
      "2026-07-28T12:00:00Z",
      "C:\\Workspace\\town\\iterion\\bots\\town-planner",
    );

    expect(
      externalActiveRuns([item], [], "C:\\Workspace\\town\\").map(
        (run) => run.id,
      ),
    ).toEqual(["windows"]);
  });
});

describe("crossStoreRunHref", () => {
  it("carries the source store so read-only run APIs resolve the right data", () => {
    const item = run("run/id", "town_planner", "2026-07-28T12:00:00Z");
    item.store_path = "/home/user/.iterion/projects/town planner";

    expect(crossStoreRunHref(item)).toBe(
      "/runs/run%2Fid?store=%2Fhome%2Fuser%2F.iterion%2Fprojects%2Ftown+planner",
    );
  });
});
