import { beforeEach, describe, expect, it, vi } from "vitest";

const api = vi.hoisted(() => ({ apiRequest: vi.fn() }));

vi.mock("./client", () => api);

import {
  bindGitCommitFiles,
  commitAssistantAuthoringGit,
  commitAssistantAuthoring,
  previewAssistantAuthoring,
  type AssistantAuthoringSnapshot,
} from "./assistantAuthoring";

const snapshot: AssistantAuthoringSnapshot = {
  editor_path: "bots/shared-planner/manifest.yaml",
  files: [
    {
      scope: "bundle",
      path: "manifest.yaml",
      size: 1,
      sha256: "a".repeat(64),
      available: true,
      readable: true,
    },
    {
      scope: "bundle",
      path: "workflows/hierarchy-epic-author.bot",
      size: 1,
      sha256: "b".repeat(64),
      available: true,
      readable: true,
    },
  ],
};

beforeEach(() => vi.resetAllMocks());

describe("authoring Git commit binding", () => {
  it("binds exact declared and editor-bundle-qualified paths to the same host files", () => {
    expect(
      bindGitCommitFiles(snapshot, [
        "manifest.yaml",
        "bots/shared-planner/workflows/hierarchy-epic-author.bot",
      ]),
    ).toEqual([
      {
        scope: "bundle",
        path: "manifest.yaml",
        expected_sha256: "a".repeat(64),
      },
      {
        scope: "bundle",
        path: "workflows/hierarchy-epic-author.bot",
        expected_sha256: "b".repeat(64),
      },
    ]);
  });

  it("sends only the attested declaration rather than the model spelling", async () => {
    api.apiRequest.mockResolvedValue({ commit: "abc", files: [] });

    await commitAssistantAuthoringGit(
      snapshot,
      ["bots/shared-planner/manifest.yaml"],
      "chore: test",
    );

    expect(api.apiRequest).toHaveBeenCalledWith(
      "/api/v1/assistant/authoring/git-commit",
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({
          editor_path: "bots/shared-planner/manifest.yaml",
          files: [
            {
              scope: "bundle",
              path: "manifest.yaml",
              expected_sha256: "a".repeat(64),
            },
          ],
          message: "chore: test",
        }),
      }),
    );
  });

  it("rejects a different bundle directory and an ambiguous mapping", () => {
    expect(() =>
      bindGitCommitFiles(snapshot, ["bots/other-planner/manifest.yaml"]),
    ).toThrow(/exactly one Git-committable file/);

    const ambiguous: AssistantAuthoringSnapshot = {
      ...snapshot,
      files: [
        ...snapshot.files,
        {
          scope: "workspace",
          path: "bots/shared-planner/manifest.yaml",
          size: 1,
          sha256: "c".repeat(64),
          available: true,
          readable: true,
        },
      ],
    };
    expect(() =>
      bindGitCommitFiles(ambiguous, ["bots/shared-planner/manifest.yaml"]),
    ).toThrow(/exactly one Git-committable file/);
  });

  it("rejects an active-only replacement target and prefers a declared Git candidate", () => {
    const activeOnly: AssistantAuthoringSnapshot = {
      editor_path: "bots/shared-planner/main.bot",
      active_file: { scope: "bundle", path: "main.bot" },
      files: [
        {
          scope: "bundle",
          path: "main.bot",
          size: 1,
          sha256: "d".repeat(64),
          available: true,
          readable: true,
          git_committable: false,
        },
      ],
    };
    expect(() => bindGitCommitFiles(activeOnly, ["main.bot"])).toThrow(
      /not manifest-declared for Git commit/,
    );

    const declaredAlternate: AssistantAuthoringSnapshot = {
      ...activeOnly,
      files: [
        ...activeOnly.files,
        {
          scope: "workspace",
          path: "bots/shared-planner/main.bot",
          size: 1,
          sha256: "e".repeat(64),
          available: true,
          readable: true,
          git_committable: true,
        },
      ],
    };
    expect(
      bindGitCommitFiles(declaredAlternate, ["bots/shared-planner/main.bot"]),
    ).toEqual([
      {
        scope: "workspace",
        path: "bots/shared-planner/main.bot",
        expected_sha256: "e".repeat(64),
      },
    ]);
  });
});

describe("authoring change binding", () => {
  const omittedSnapshot: AssistantAuthoringSnapshot = {
    editor_path: "bots/planner/main.bot",
    version: 4,
    files: [
      {
        scope: "bundle",
        path: "manifest.yaml",
        size: 1,
        sha256: "a".repeat(64),
        available: true,
        readable: true,
      },
    ],
  };
  const omittedChange = {
    scope: "workspace" as const,
    path: "schemas/vertical/planner-plan-revision-receipt.schema.json",
    replacements: [{ before: "old", after: "new" }],
  };
  const freshSnapshot: AssistantAuthoringSnapshot = {
    ...omittedSnapshot,
    files: [
      ...omittedSnapshot.files,
      {
        scope: "workspace",
        path: omittedChange.path,
        size: 3,
        sha256: "c".repeat(64),
        available: true,
        readable: true,
      },
    ],
  };
  const snapshotForTest = (): AssistantAuthoringSnapshot => ({
    ...omittedSnapshot,
    files: omittedSnapshot.files.map((file) => ({ ...file })),
  });

  it("refreshes once for an omitted local manifest file and uses its host hash", async () => {
    const current = snapshotForTest();
    api.apiRequest
      .mockResolvedValueOnce(freshSnapshot)
      .mockResolvedValueOnce({ files: [], saved: false });

    await previewAssistantAuthoring(current, [omittedChange]);

    expect(api.apiRequest).toHaveBeenCalledTimes(2);
    expect(api.apiRequest).toHaveBeenNthCalledWith(
      1,
      "/api/v1/assistant/authoring/snapshot",
      expect.objectContaining({
        method: "POST",
        body: JSON.stringify({ editor_path: current.editor_path }),
      }),
    );
    const request = api.apiRequest.mock.calls[1]?.[1] as { body: string };
    expect(JSON.parse(request.body).changes).toEqual([
      { ...omittedChange, expected_sha256: "c".repeat(64) },
    ]);
  });

  it("refreshes and binds an explicitly declared missing local create without a hash", async () => {
    const current = snapshotForTest();
    const create = {
      scope: "workspace" as const,
      path: "iterion/vertical/planner-input-r4.json",
      create: { content: '{"run_id":"tabarria-v1-epics-r4"}\n' },
    };
    api.apiRequest
      .mockResolvedValueOnce({
        ...current,
        files: [
          ...current.files,
          {
            scope: "workspace",
            path: create.path,
            size: 0,
            available: false,
            readable: false,
            is_manifest_declared: true,
            reason: "declared_missing_local_file",
          },
        ],
      })
      .mockResolvedValueOnce({ files: [], saved: false });

    await previewAssistantAuthoring(current, [create]);

    expect(api.apiRequest).toHaveBeenCalledTimes(2);
    const request = api.apiRequest.mock.calls[1]?.[1] as { body: string };
    expect(JSON.parse(request.body).changes).toEqual([create]);
  });

  it("rejects creates that are available, cloud-backed, or not manifest-declared", async () => {
    const create = {
      scope: "workspace" as const,
      path: "iterion/vertical/planner-input-r4.json",
      create: { content: "{}" },
    };
    for (const file of [
      {
        scope: "workspace" as const,
        path: create.path,
        size: 2,
        sha256: "d".repeat(64),
        available: true,
        readable: true,
        is_manifest_declared: true,
      },
      {
        scope: "workspace" as const,
        path: create.path,
        size: 0,
        available: false,
        readable: false,
        is_manifest_declared: false,
        reason: "declared_missing_local_file",
      },
    ]) {
      const current = snapshotForTest();
      api.apiRequest.mockResolvedValueOnce({ ...current, files: [...current.files, file] });
      await expect(previewAssistantAuthoring(current, [create])).rejects.toThrow(
        /explicitly declared missing local/,
      );
      vi.resetAllMocks();
    }

    const cloud = { ...snapshotForTest(), editor_path: "botsource://team/planner/main.bot" };
    await expect(previewAssistantAuthoring(cloud, [create])).rejects.toThrow(
      /local project editor/,
    );
    expect(api.apiRequest).not.toHaveBeenCalled();
  });

  it("reuses the one refresh for commit and keeps captured hashes for present files", async () => {
    const current = snapshotForTest();
    const changes = [
      {
        scope: "bundle" as const,
        path: "manifest.yaml",
        replacements: [{ before: "old", after: "new" }],
      },
      omittedChange,
    ];
    api.apiRequest
      .mockResolvedValueOnce(freshSnapshot)
      .mockResolvedValueOnce({ files: [], saved: false })
      .mockResolvedValueOnce({ files: [], saved: true });

    await previewAssistantAuthoring(current, changes);
    await commitAssistantAuthoring(current, changes);

    expect(api.apiRequest).toHaveBeenCalledTimes(3);
    const previewBody = JSON.parse(
      (api.apiRequest.mock.calls[1]?.[1] as { body: string }).body,
    );
    const commitBody = JSON.parse(
      (api.apiRequest.mock.calls[2]?.[1] as { body: string }).body,
    );
    expect(previewBody.changes).toEqual(commitBody.changes);
    expect(previewBody.changes).toEqual([
      {
        scope: "bundle",
        path: "manifest.yaml",
        replacements: [{ before: "old", after: "new" }],
        expected_sha256: "a".repeat(64),
      },
      { ...omittedChange, expected_sha256: "c".repeat(64) },
    ]);
  });

  it("does not refresh cloud bot-source paths", async () => {
    const cloud = {
      ...snapshotForTest(),
      editor_path: "botsource://team/planner/main.bot",
    };
    await expect(previewAssistantAuthoring(cloud, [omittedChange])).rejects.toThrow(
      /current authoring snapshot/,
    );
    expect(api.apiRequest).not.toHaveBeenCalled();
  });

  it("rejects a refresh that resolves to another editor path", async () => {
    const current = snapshotForTest();
    api.apiRequest.mockResolvedValueOnce({
      ...freshSnapshot,
      editor_path: "bots/other/main.bot",
    });

    await expect(
      previewAssistantAuthoring(current, [omittedChange]),
    ).rejects.toThrow(/changed editor path/);
    expect(api.apiRequest).toHaveBeenCalledTimes(1);
  });
});
