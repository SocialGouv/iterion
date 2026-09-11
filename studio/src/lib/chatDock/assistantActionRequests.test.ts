// @vitest-environment jsdom

import { beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({
  transitionIssue: vi.fn(),
  commitAssistantAuthoringGit: vi.fn(),
  publishAssistantAuthoringGit: vi.fn(),
  updateAssistantBotDependency: vi.fn(),
  localizeAssistantBotDependency: vi.fn(),
  resumeRun: vi.fn(),
  getServerInfo: vi.fn(),
  rewindRun: vi.fn(),
  createAssistantRunWatch: vi.fn(),
  getBot: vi.fn(),
  createBot: vi.fn(),
  createRun: vi.fn(),
  resetPipelineTask: vi.fn(),
  createWorkspaceHandoff: vi.fn(),
  completeWorkspaceHandoff: vi.fn(),
}));

vi.mock("@/api/native", () => api);
vi.mock("@/api/assistantAuthoring", () => ({
  commitAssistantAuthoringGit: api.commitAssistantAuthoringGit,
  publishAssistantAuthoringGit: api.publishAssistantAuthoringGit,
}));
vi.mock("@/api/assistantDependencies", () => ({
  updateAssistantBotDependency: api.updateAssistantBotDependency,
  localizeAssistantBotDependency: api.localizeAssistantBotDependency,
}));
vi.mock("@/api/bots", () => ({
  getBot: api.getBot,
  createBot: api.createBot,
}));
vi.mock("@/api/dispatcher", () => ({}));
vi.mock("@/api/pipelineBoards", () => ({
  resetPipelineTask: api.resetPipelineTask,
}));
vi.mock("@/api/plugins", () => ({}));
vi.mock("@/api/workspaceHandoff", () => ({
  createWorkspaceHandoff: api.createWorkspaceHandoff,
  completeWorkspaceHandoff: api.completeWorkspaceHandoff,
}));
vi.mock("@/api/runs", () => ({
  resumeRun: api.resumeRun,
  getServerInfo: api.getServerInfo,
  rewindRun: api.rewindRun,
  createAssistantRunWatch: api.createAssistantRunWatch,
  createRun: api.createRun,
}));

import type { AssistantActionRequest } from "./assistantActions";
import {
  executeAssistantAction,
  validateAssistantActionRequest,
} from "./assistantActionRequests";

beforeEach(() => vi.resetAllMocks());

function request(
  id: AssistantActionRequest["id"],
  args: Record<string, unknown>,
): AssistantActionRequest {
  return { key: "run:node:1:0", id, intent: "explicit", args };
}

describe("assistant action request boundary", () => {
  it("binds a new project handoff to the host-selected source run", async () => {
    api.createWorkspaceHandoff.mockResolvedValue({
      ticket: "ticket-1",
      handoff_id: "handoff-1",
      destination_project_id: "target-project",
      destination_project_name: "Target",
    });
    const postMessage = vi.spyOn(window.parent, "postMessage");
    const validated = validateAssistantActionRequest(
      request("workspace.handoff", {
        destination_project: "target-project",
        summary: "Update the shared bot",
        source_run_id: "model-cannot-bind-this",
      }),
    );
    expect(validated.args).not.toHaveProperty("source_run_id");

    await executeAssistantAction(validated, { assistantRunId: "source-run" });

    expect(api.createWorkspaceHandoff).toHaveBeenCalledWith(
      "target-project",
      "Update the shared bot",
      "source-run",
    );
    expect(postMessage).toHaveBeenCalledWith(
      expect.objectContaining({
        type: "workspace-handoff",
        projectId: "target-project",
        ticket: "ticket-1",
      }),
      window.location.origin,
    );
  });

  it("reports only a bounded handoff receipt through host-bound ids", async () => {
    api.completeWorkspaceHandoff.mockResolvedValue({
      handoff_id: "handoff-1",
      receipt_id: "receipt-1",
      delivery_status: "pending",
      idempotent: false,
    });
    const validated = validateAssistantActionRequest(
      request("workspace.handoff.complete", {
        status: "completed",
        summary: "Shared bot updated and verified",
        changed_files: ["bots/shared/main.bot"],
        commit_sha: "a".repeat(40),
        branch: "fix/shared-bot",
        pr_url: "https://github.com/SocialGouv/iterion/pull/1125",
        next_step: "Resume the source task",
        handoff_id: "model-handoff",
        destination_run_id: "model-run",
        secret: "must not cross",
      }),
    );
    expect(validated.args).not.toHaveProperty("handoff_id");
    expect(validated.args).not.toHaveProperty("destination_run_id");
    expect(validated.args).not.toHaveProperty("secret");

    await executeAssistantAction(validated, {
      assistantRunId: "target-run",
      workspaceHandoffId: "handoff-1",
    });

    expect(api.completeWorkspaceHandoff).toHaveBeenCalledWith(
      "handoff-1",
      "target-run",
      validated.args,
    );
  });

  it("rejects unsafe handoff completion artifacts", () => {
    for (const args of [
      { status: "working", summary: "not terminal" },
      { status: "completed", summary: "ok", changed_files: ["../secret"] },
      { status: "completed", summary: "ok", commit_sha: "abc123" },
      {
        status: "completed",
        summary: "ok",
        pr_url: "https://evil.test/project/pull/1",
      },
    ]) {
      expect(() =>
        validateAssistantActionRequest(
          request("workspace.handoff.complete", args),
        ),
      ).toThrow();
    }
  });

  it("rebuilds API payloads from allowed fields only", () => {
    const validated = validateAssistantActionRequest(
      request("board.issue.update", {
        issue_id: "issue-1",
        title: "New title",
        labels: ["bug"],
        method: "DELETE",
        secret: "must not cross",
      }),
    );
    expect(validated.args).toEqual({
      issue_id: "issue-1",
      patch: { title: "New title", labels: ["bug"] },
    });
  });

  it("rejects incomplete and editor-session-bypassing requests", () => {
    expect(() =>
      validateAssistantActionRequest(request("run.cancel", {})),
    ).toThrow(/run_id/);
    expect(() =>
      validateAssistantActionRequest(request("editor.save", {})),
    ).toThrow(/live editor-session/i);
  });

  it("accepts exactly one catalog bot or verified workflow file for run.launch", () => {
    expect(
      validateAssistantActionRequest(
        request("run.launch", { file_path: "bots/animals/animal-range.bot" }),
      ).args,
    ).toEqual({ file_path: "bots/animals/animal-range.bot" });
    expect(() =>
      validateAssistantActionRequest(request("run.launch", {})),
    ).toThrow(/requires bot or file_path/);
    expect(() =>
      validateAssistantActionRequest(
        request("run.launch", {
          bot: "bestof-lab",
          file_path: "bots/animals/animal-range.bot",
        }),
      ),
    ).toThrow(/either bot or file_path/);
    expect(() =>
      validateAssistantActionRequest(
        request("run.launch", {
          file_path: "bots/animals/animal-range.bot",
          source_run_id: "failed-1",
        }),
      ),
    ).toThrow(/cannot be combined with source_run_id/);
  });

  it("launches a local workflow file and arms its assistant watch", async () => {
    api.getServerInfo.mockResolvedValue({
      mode: "local",
      work_dir: "/workspace/project",
    });
    api.createRun.mockResolvedValue({ run_id: "animal-run-1", status: "running" });
    api.createAssistantRunWatch.mockResolvedValue({ id: "watch-animal", target_run_id: "animal-run-1" });
    const validated = validateAssistantActionRequest(
      request("run.launch", {
        file_path: "bots/bestof-lab/animal-range.bot",
        vars: { range_id: "universel", version: "1" },
      }),
    );

    await executeAssistantAction(validated, { assistantRunId: "copi-1" });

    expect(api.createRun).toHaveBeenCalledWith({
      file_path: "bots/bestof-lab/animal-range.bot",
      vars: { range_id: "universel", version: "1" },
    });
    expect(api.createAssistantRunWatch).toHaveBeenCalledWith("animal-run-1", {
      assistant_run_id: "copi-1",
      mode: "propose",
      kinds: ["run.paused", "run.failed", "run.stalled", "run.finished"],
      cooldown_seconds: 0,
    });
  });

  it("rejects a local launch path outside the workspace or without a bot suffix", async () => {
    api.getServerInfo.mockResolvedValue({
      mode: "local",
      work_dir: "/workspace/project",
    });
    for (const filePath of ["../outside/animal.bot", "bots/bestof-lab/README.md"]) {
      const validated = validateAssistantActionRequest(
        request("run.launch", { file_path: filePath }),
      );
      await expect(executeAssistantAction(validated)).rejects.toThrow(
        /\.bot file inside the active local work directory/,
      );
    }
    expect(api.createRun).not.toHaveBeenCalled();
  });

  it("accepts integer pipeline priorities and an omitted priority", () => {
    const prioritized = validateAssistantActionRequest(
      request("pipeline.task.create", {
        bot: "dev-squad",
        title: "Fix candidate authority",
        priority: 20,
      }),
    );
    expect(prioritized.args.priority).toBe(20);

    const unprioritized = validateAssistantActionRequest(
      request("pipeline.task.create", {
        bot: "dev-squad",
        title: "Fix candidate authority",
      }),
    );
    expect(unprioritized.args).not.toHaveProperty("priority");
  });

  it.each([
    ["high", '"high"'],
    [1.5, "1.5"],
    [Number.NaN, "NaN"],
    [Number.POSITIVE_INFINITY, "Infinity"],
  ])("rejects non-integer priority %s", (priority, displayed) => {
    expect(() =>
      validateAssistantActionRequest(
        request("pipeline.task.create", {
          bot: "dev-squad",
          title: "Fix candidate authority",
          priority,
        }),
      ),
    ).toThrow(
      `priority must be an integer, for example 10 or 20; received ${displayed}`,
    );
  });

  it.each([
    ["omitted", undefined],
    ["false", false],
    ["true", true],
  ])("validates pipeline.task.reset fresh:%s", (_label, fresh) => {
    const source = { task_id: "task-1", ...(fresh === undefined ? {} : { fresh }) };
    const validated = validateAssistantActionRequest(
      request("pipeline.task.reset", source),
    );
    expect(validated.args).toEqual({
      task_id: "task-1",
      ...(fresh === undefined ? {} : { fresh }),
    });
  });

  it("rejects a non-boolean pipeline.task.reset fresh value", () => {
    expect(() =>
      validateAssistantActionRequest(
        request("pipeline.task.reset", { task_id: "task-1", fresh: "true" }),
      ),
    ).toThrow("fresh must be true or false");
  });

  it.each([
    ["omitted", undefined, false],
    ["false", false, false],
    ["true", true, true],
  ])("executes pipeline.task.reset fresh:%s", async (_label, fresh, expected) => {
    api.resetPipelineTask.mockResolvedValue(undefined);
    const validated = validateAssistantActionRequest(
      request("pipeline.task.reset", {
        task_id: "task-1",
        ...(fresh === undefined ? {} : { fresh }),
      }),
    );

    await expect(executeAssistantAction(validated)).resolves.toEqual({
      message: "Reset pipeline task task-1",
    });
    expect(api.resetPipelineTask).toHaveBeenCalledWith("task-1", {
      fresh: expected,
    });
  });

  it("executes the host-selected function with validated arguments", async () => {
    api.transitionIssue.mockResolvedValue({ id: "issue-1", state: "done" });
    const validated = validateAssistantActionRequest(
      request("board.issue.transition", {
        issue_id: "issue-1",
        to: "done",
        arbitrary_request_options: { credentials: "omit" },
      }),
    );
    await expect(executeAssistantAction(validated)).resolves.toMatchObject({
      message: expect.stringContaining("issue-1"),
    });
    expect(api.transitionIssue).toHaveBeenCalledWith("issue-1", "done");
  });

  it("attests the local editor path after creating a bot bundle", async () => {
    api.createBot.mockResolvedValue({
      name: "expense-approval",
      display_name: "Expense approval",
      rel_path: "bots/expense-approval",
      path: "/host-owned/workspace/bots/expense-approval",
    });
    const validated = validateAssistantActionRequest(
      request("bot.create", {
        slug: "expense-approval",
        instructions: "Route expense approval requests.",
      }),
    );

    await expect(executeAssistantAction(validated)).resolves.toEqual({
      message: "Created bot Expense approval",
      href: "/editor?file=bots%2Fexpense-approval%2Fmain.bot",
      hrefLabel: "Open bot",
      receipt: {
        created_bot: {
          name: "expense-approval",
          editor_path: "bots/expense-approval/main.bot",
        },
      },
    });
    expect(api.createBot).toHaveBeenCalledWith({
      slug: "expense-approval",
      instructions: "Route expense approval requests.",
    });
  });

  it("commits only snapshot-bound declared authoring files and returns an exact host receipt", async () => {
    const snapshot = {
      editor_path: "bots/shared-planner/manifest.yaml",
      files: [
        {
          scope: "bundle" as const,
          path: "workflows/hierarchy-epic-author.bot",
          size: 10,
          sha256: "a".repeat(64),
          available: true,
          readable: true,
        },
      ],
    };
    api.commitAssistantAuthoringGit.mockResolvedValue({
      commit: "0123456789abcdef0123456789abcdef01234567",
      files: ["bots/shared-planner/workflows/hierarchy-epic-author.bot"],
    });
    const validated = validateAssistantActionRequest(
      request("authoring.git.commit", {
        editor_session_id: "editor-session-1",
        editor_revision: 4,
        files: ["workflows/hierarchy-epic-author.bot"],
        message: "fix(planner): make hierarchy target explicit",
        repository: "/model-selected/repository",
        amend: true,
      }),
    );

    expect(validated.args).toEqual({
      editor_session_id: "editor-session-1",
      editor_revision: 4,
      files: ["workflows/hierarchy-epic-author.bot"],
      message: "fix(planner): make hierarchy target explicit",
    });
    await expect(
      executeAssistantAction(validated, { authoringSnapshot: snapshot }),
    ).resolves.toEqual({
      message: "Created commit 0123456789ab",
      receipt: { git_commit: "0123456789abcdef0123456789abcdef01234567" },
    });
    expect(api.commitAssistantAuthoringGit).toHaveBeenCalledWith(
      snapshot,
      ["workflows/hierarchy-epic-author.bot"],
      "fix(planner): make hierarchy target explicit",
    );

    api.commitAssistantAuthoringGit.mockResolvedValue({
      commit: "0123456789abcdef",
      files: ["bots/shared-planner/workflows/hierarchy-epic-author.bot"],
    });
    await expect(
      executeAssistantAction(validated, { authoringSnapshot: snapshot }),
    ).resolves.toEqual({ message: "Created commit 0123456789ab" });
  });

  it("rejects authoring commits without one current relative file", () => {
    expect(() =>
      validateAssistantActionRequest(
        request("authoring.git.commit", {
          editor_session_id: "editor-session-1",
          editor_revision: 4,
          files: ["../outside.bot"],
          message: "fix: unsafe",
        }),
      ),
    ).toThrow(/relative authoring paths/);
    expect(() =>
      validateAssistantActionRequest(
        request("authoring.git.commit", {
          editor_session_id: "editor-session-1",
          editor_revision: 4.5,
          files: ["workflows/hierarchy-epic-author.bot"],
          message: "fix: bad revision",
        }),
      ),
    ).toThrow(/editor_revision/);
  });

  it("publishes only the current commit to a bounded fresh branch", async () => {
    const snapshot = { editor_path: "bots/shared-planner/manifest.yaml", files: [] };
    api.publishAssistantAuthoringGit.mockResolvedValue({
      commit: "0123456789abcdef",
      branch: "shared-planner/v0.3.1",
    });
    const validated = validateAssistantActionRequest(
      request("authoring.git.publish", {
        editor_session_id: "editor-session-1",
        editor_revision: 4,
        commit: "0123456789ab",
        branch: "shared-planner/v0.3.1",
        remote: "attacker",
        force: true,
      }),
    );
    expect(validated.args).toEqual({
      editor_session_id: "editor-session-1",
      editor_revision: 4,
      commit: "0123456789ab",
      branch: "shared-planner/v0.3.1",
    });
    await executeAssistantAction(validated, { authoringSnapshot: snapshot });
    expect(api.publishAssistantAuthoringGit).toHaveBeenCalledWith(
      snapshot,
      "0123456789ab",
      "shared-planner/v0.3.1",
    );
    expect(() => validateAssistantActionRequest(request("authoring.git.publish", {
      editor_session_id: "editor-session-1", editor_revision: 4,
      commit: "0123456789ab", branch: "bad:ref",
    }))).toThrow(/branch is invalid/);
  });

  it("updates one exact pinned dependency without exposing source or Git args", async () => {
    const ref = "a".repeat(40);
    api.updateAssistantBotDependency.mockResolvedValue({
      name: "shared-planner",
      previous_ref: "b".repeat(40),
      ref,
      bundle_sha256: "c".repeat(64),
      commit: "0123456789abcdef",
      installed_path: ".botz/shared-planner",
    });
    const validated = validateAssistantActionRequest(request("dependency.bots.update", {
      name: "shared-planner", ref, message: "chore(bots): pin shared planner",
      source: "attacker", workdir: "/model-picked", force: true, allow_dirty: true,
    }));
    expect(validated.args).toEqual({
      name: "shared-planner", ref, message: "chore(bots): pin shared planner",
    });
    await expect(executeAssistantAction(validated)).resolves.toMatchObject({
      message: expect.stringContaining("shared-planner"),
    });
    expect(api.updateAssistantBotDependency).toHaveBeenCalledWith(
      "shared-planner", ref, "chore(bots): pin shared planner",
    );
    expect(() => validateAssistantActionRequest(request("dependency.bots.update", {
      name: "shared-planner", ref: "A".repeat(40), message: "bad",
    }))).toThrow(/40-character/);
  });

  it("localizes one dependency only through the active consumer snapshot", async () => {
    const snapshot = { editor_path: "bots/planner/main.bot", files: [] };
    api.localizeAssistantBotDependency.mockResolvedValue({
      name: "shared-planner",
      local_source: "bots/planner/plugins/shared-planner",
      source_commit: "a".repeat(40),
      lock_commit: "b".repeat(40),
      bundle_sha256: "c".repeat(64),
      installed_path: ".botz/shared-planner",
      resumed_after_import: false,
    });
    const validated = validateAssistantActionRequest(request("dependency.bots.localize", {
      name: "shared-planner",
      message: "chore(planner): vendor shared planner source",
      editor_session_id: "editor-session-1",
      editor_revision: 4,
      destination: "/model-picked",
      source: "git@attacker",
      ref: "a".repeat(40),
      force: true,
    }));
    expect(validated.args).toEqual({
      name: "shared-planner",
      message: "chore(planner): vendor shared planner source",
      editor_session_id: "editor-session-1",
      editor_revision: 4,
    });
    await expect(executeAssistantAction(validated, { authoringSnapshot: snapshot })).resolves.toMatchObject({
      message: expect.stringContaining("bots/planner/plugins/shared-planner"),
    });
    expect(api.localizeAssistantBotDependency).toHaveBeenCalledWith(
      snapshot,
      "shared-planner",
      "chore(planner): vendor shared planner source",
    );
    await expect(executeAssistantAction(validated)).rejects.toThrow(/authoring editor session/i);
    expect(() => validateAssistantActionRequest(request("dependency.bots.localize", {
      name: "../shared-planner", message: "bad", editor_session_id: "editor-session-1", editor_revision: 4,
    }))).toThrow(/name is invalid/);
  });

  it("arms the assistant on a resumed run's full supervised lifecycle", async () => {
    // A resume is proposed to make a run PROGRESS. Without a watch it is a
    // one-way instruction: the run fails again minutes later and the
    // assistant learns nothing until the operator comes back to ask. That is
    // the difference between "it resumed the run" and "it is seeing this
    // through" — and it is the same convention run.launch already follows.
    api.resumeRun.mockResolvedValue({ run_id: "run-1", status: "running" });
    api.createAssistantRunWatch.mockResolvedValue({ id: "w-1", target_run_id: "run-1" });
    const validated = validateAssistantActionRequest(
      request("run.resume", { run_id: "run-1" }),
    );

    await executeAssistantAction(validated, { assistantRunId: "assistant-1" });

    expect(api.createAssistantRunWatch).toHaveBeenCalledWith("run-1", {
      assistant_run_id: "assistant-1",
      mode: "propose",
      kinds: ["run.paused", "run.failed", "run.stalled", "run.finished"],
      cooldown_seconds: 0,
    });

    await executeAssistantAction(validated, {
      assistantRunId: "assistant-1",
      forceResume: true,
    });
    expect(api.createAssistantRunWatch).toHaveBeenNthCalledWith(2, "run-1", {
      assistant_run_id: "assistant-1",
      mode: "propose",
      kinds: ["run.paused", "run.failed", "run.stalled", "run.finished"],
      cooldown_seconds: 0,
    });
  });

  it("still reports a successful resume when the watch cannot be armed", async () => {
    // Losing the follow-up must not turn a resume that WORKED into a
    // reported failure: the run is going either way.
    api.resumeRun.mockResolvedValue({ run_id: "run-1", status: "running" });
    api.createAssistantRunWatch.mockRejectedValue(new Error("watch store down"));
    const validated = validateAssistantActionRequest(
      request("run.resume", { run_id: "run-1" }),
    );

    await expect(
      executeAssistantAction(validated, { assistantRunId: "assistant-1" }),
    ).resolves.toMatchObject({ message: expect.stringContaining("run-1") });
  });

  it("strips model-supplied force and only force-resumes from host context", async () => {
    api.resumeRun.mockResolvedValue({ run_id: "run-1", status: "running" });
    const validated = validateAssistantActionRequest(
      request("run.resume", { run_id: "run-1", force: true }),
    );
    expect(validated.args).toEqual({ run_id: "run-1" });

    await executeAssistantAction(validated);
    expect(api.resumeRun).toHaveBeenNthCalledWith(1, "run-1", {});

    await executeAssistantAction(validated, { forceResume: true });
    expect(api.resumeRun).toHaveBeenNthCalledWith(2, "run-1", {
      force: true,
    });
  });

  it("accepts a host-checked local file path for resume and shows it in the action detail", async () => {
    api.resumeRun.mockResolvedValue({ run_id: "run-1", status: "running" });
    api.getServerInfo.mockResolvedValue({
      mode: "local",
      work_dir: "/workspace/project",
    });
    const validated = validateAssistantActionRequest(
      request("run.resume", {
        run_id: "run-1",
        file_path: "bots/animals/main.bot",
      }),
    );

    expect(validated.args).toEqual({
      run_id: "run-1",
      file_path: "bots/animals/main.bot",
    });
    expect(validated.detail).toContain("bots/animals/main.bot");
    await executeAssistantAction(validated);
    expect(api.resumeRun).toHaveBeenCalledWith("run-1", {
      file_path: "bots/animals/main.bot",
    });
  });

  it("rejects an assistant resume path outside the active local workspace", async () => {
    api.getServerInfo.mockResolvedValue({
      mode: "local",
      work_dir: "/workspace/project",
    });
    const validated = validateAssistantActionRequest(
      request("run.resume", {
        run_id: "run-1",
        file_path: "../outside/main.bot",
      }),
    );

    await expect(executeAssistantAction(validated)).rejects.toThrow(
      /inside the active local work directory/,
    );
    expect(api.resumeRun).not.toHaveBeenCalled();
  });

  it("rewinds through the host API without granting file restoration authority", async () => {
    api.rewindRun.mockResolvedValue({
      run_id: "run-1",
      node_id: "establish_visual_foundation",
      dropped_nodes: ["establish_visual_foundation", "review"],
      status: "cancelled",
    });
    const validated = validateAssistantActionRequest(
      request("run.rewind", {
        run_id: "run-1",
        auto: true,
        restore_scope: "full",
        source_path: "/model/chosen/workflow.bot",
        keep_files: false,
      }),
    );
    expect(validated.args).toEqual({
      run_id: "run-1",
      auto: true,
      restore_scope: "none",
    });

    await expect(executeAssistantAction(validated)).resolves.toMatchObject({
      message: expect.stringContaining("invalidated 2 nodes"),
      href: "/runs/run-1",
    });
    expect(api.rewindRun).toHaveBeenCalledWith("run-1", {
      auto: true,
      restore_scope: "none",
    });
  });

  it("requires exactly one assistant rewind target", () => {
    expect(() =>
      validateAssistantActionRequest(
        request("run.rewind", { run_id: "run-1" }),
      ),
    ).toThrow(/requires auto:true or node_id/);
    expect(() =>
      validateAssistantActionRequest(
        request("run.rewind", {
          run_id: "run-1",
          auto: true,
          node_id: "review",
        }),
      ),
    ).toThrow(/either auto:true or node_id, not both/);
  });

  it("passes an explicit rewind node through the bounded payload", async () => {
    api.rewindRun.mockResolvedValue({
      run_id: "run-2",
      node_id: "review",
      dropped_nodes: ["review"],
      status: "cancelled",
    });
    const validated = validateAssistantActionRequest(
      request("run.rewind", { run_id: "run-2", node_id: "review" }),
    );

    await executeAssistantAction(validated);
    expect(api.rewindRun).toHaveBeenCalledWith("run-2", {
      node_id: "review",
      restore_scope: "none",
    });
  });

  it("binds run.watch to the host-selected assistant run", async () => {
    api.createAssistantRunWatch.mockResolvedValue({
      id: "watch-1",
      target_run_id: "target-1",
    });
    const validated = validateAssistantActionRequest(
      request("run.watch", {
        target_run_id: "target-1",
        mode: "propose",
        max_episodes: 4,
        assistant_run_id: "model-chosen-run",
      }),
    );
    expect(validated.args).not.toHaveProperty("assistant_run_id");
    expect(validated.args).not.toHaveProperty("max_episodes");
    await executeAssistantAction(validated, { assistantRunId: "assistant-1" });
    expect(api.createAssistantRunWatch).toHaveBeenCalledWith("target-1", {
      assistant_run_id: "assistant-1",
      mode: "propose",
      cooldown_seconds: undefined,
    });
  });

  it("reports descendant coverage by the existing root watch", async () => {
    api.createAssistantRunWatch.mockResolvedValue({
      id: "watch-root",
      target_run_id: "root-1",
      covered_run_id: "child-1",
    });
    const validated = validateAssistantActionRequest(
      request("run.watch", {
        target_run_id: "child-1",
        mode: "propose",
        kinds: ["run.failed", "run.paused"],
      }),
    );
    await expect(
      executeAssistantAction(validated, { assistantRunId: "assistant-1" }),
    ).resolves.toMatchObject({
      message: expect.stringContaining("already covered by root watch watch-root"),
    });
  });

  it("forwards the selected paused-gate watch outcome", async () => {
    api.createAssistantRunWatch.mockResolvedValue({
      id: "watch-paused",
      target_run_id: "target-1",
    });
    const validated = validateAssistantActionRequest(
      request("run.watch", {
        target_run_id: "target-1",
        mode: "propose",
        kinds: ["run.failed", "run.paused"],
      }),
    );
    await executeAssistantAction(validated, { assistantRunId: "assistant-1" });
    expect(api.createAssistantRunWatch).toHaveBeenCalledWith("target-1", {
      assistant_run_id: "assistant-1",
      mode: "propose",
      cooldown_seconds: undefined,
      kinds: ["run.failed", "run.paused"],
    });
  });

  it("rejects unsupported or duplicate run.watch outcomes", () => {
    expect(() =>
      validateAssistantActionRequest(
        request("run.watch", { target_run_id: "target-1", kinds: ["run.paused", "run.paused"] }),
      ),
    ).toThrow(/kinds must not contain duplicates/);
    expect(() =>
      validateAssistantActionRequest(
        request("run.watch", { target_run_id: "target-1", kinds: ["run.paused", "run.unknown"] }),
      ),
    ).toThrow(/supported run outcomes/);
  });

  it("rejects autonomous repair modes", () => {
    expect(() =>
      validateAssistantActionRequest(
        request("run.watch", { target_run_id: "target-1", mode: "auto_safe" }),
      ),
    ).toThrow(/diagnose or propose/);
  });

  it("delegates a failed run and watches the worker outcome", async () => {
    api.getBot.mockResolvedValue({
      name: "feature-dev",
      display_name: "Featurly",
      path: "bots/feature-dev/main.bot",
      vars: {
        fields: [
          { name: "failure_context" },
          { name: "delegation_instructions" },
        ],
      },
    });
    api.createRun.mockResolvedValue({ run_id: "repair-abc-1", status: "running" });
    api.createAssistantRunWatch.mockResolvedValue({ id: "watch-worker", target_run_id: "repair-abc-1" });
    const validated = validateAssistantActionRequest(
      request("run.launch", {
        bot: "feature-dev",
        source_run_id: "failed-1",
        instructions: "Fix the failing validation and add a regression test.",
        vars: { feature_prompt: "Repair the failed bot" },
        repo_path: "/model/must/not/choose",
      }),
    );
    await executeAssistantAction(validated, { assistantRunId: "copi-1" });
    expect(api.createRun).toHaveBeenCalledWith({
      file_path: "bots/feature-dev/main.bot",
      bot_id: "feature-dev",
      source_run_id: "failed-1",
      instructions: "Fix the failing validation and add a regression test.",
      vars: { feature_prompt: "Repair the failed bot" },
    });
    expect(api.createAssistantRunWatch).toHaveBeenCalledWith("repair-abc-1", {
      assistant_run_id: "copi-1",
      mode: "propose",
      kinds: ["run.paused", "run.failed", "run.stalled", "run.finished"],
      cooldown_seconds: 0,
    });
  });

  it("rejects an original workflow used as a delegated repair worker", async () => {
    api.getBot.mockResolvedValue({
      name: "legendary-film-chapter",
      path: "bots/legendary-film-chapter/main.bot",
      vars: { fields: [{ name: "chapter_id" }] },
    });
    const validated = validateAssistantActionRequest(
      request("run.launch", {
        bot: "legendary-film-chapter",
        source_run_id: "failed-1",
        instructions: "Repair the timing precondition.",
      }),
    );

    await expect(executeAssistantAction(validated)).rejects.toThrow(
      /not a repair worker/i,
    );
    expect(api.createRun).not.toHaveBeenCalled();
  });
});
