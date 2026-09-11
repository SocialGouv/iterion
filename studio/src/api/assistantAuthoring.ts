import { apiRequest } from "./client";

const BASE = "/api/v1/assistant/authoring";

export interface AssistantAuthoringFileSnapshot {
  scope: "bundle" | "workspace";
  path: string;
  size: number;
  sha256?: string;
  available: boolean;
  /** True when Copi can resolve the metadata path with its local read tool. */
  readable: boolean;
  /** False for an active-editor-only replacement target that the manifest did
   * not grant to the separate Git commit capability. Older servers omit it. */
  git_committable?: boolean;
  /** True only when this exact path is declared by authoring.editable_files.
   * Active-editor and bootstrap conveniences never set this capability. */
  is_manifest_declared?: boolean;
  /** Explicit cloud attachment enrichment; absent from the server snapshot. */
  complete?: boolean;
  source?: string;
  reason?: string;
}

export interface AssistantAuthoringSnapshot {
  editor_path: string;
  version?: number;
  files: AssistantAuthoringFileSnapshot[];
  /** Host-attested bundle-relative identity of the active editor file when
   * it is itself declared in the authoring perimeter. */
  active_file?: {
    scope: "bundle";
    path: string;
  };
}

export interface AssistantFileReplacement {
  before: string;
  after: string;
}

interface AssistantFileChangeTarget {
  scope: "bundle" | "workspace";
  path: string;
}

export type AssistantFileChange = AssistantFileChangeTarget & (
  | { replacements: AssistantFileReplacement[]; create?: never }
  | { create: { content: string }; replacements?: never }
);

export interface AssistantAuthoringPreviewFile {
  scope: "bundle" | "workspace";
  path: string;
  operation?: "replace" | "create";
  before: string;
  after: string;
}

export interface AssistantAuthoringValidationCheck {
  kind: "bot_compile" | "python_syntax" | "json_syntax";
  path: string;
  status: "passed";
  message?: string;
}

export interface AssistantAuthoringValidation {
  checks: AssistantAuthoringValidationCheck[];
  behavioral_tests: "not_run";
}

export interface AssistantAuthoringResult {
  files: AssistantAuthoringPreviewFile[];
  version?: number;
  saved: boolean;
  validation?: AssistantAuthoringValidation;
}

export interface AssistantAuthoringGitCommitResult {
  commit: string;
  files: string[];
}

export interface AssistantAuthoringGitPublishResult {
  commit: string;
  branch: string;
}

type BoundChange = AssistantFileChange & { expected_sha256?: string };

export function snapshotAssistantAuthoring(
  editorPath: string,
): Promise<AssistantAuthoringSnapshot> {
  return apiRequest(`${BASE}/snapshot`, {
    method: "POST",
    body: JSON.stringify({ editor_path: editorPath }),
  });
}

function isLocalProjectEditorPath(editorPath: string): boolean {
  return !editorPath.startsWith("botsource://");
}

function findSnapshotFile(
  snapshot: AssistantAuthoringSnapshot,
  change: AssistantFileChange,
): AssistantAuthoringFileSnapshot | undefined {
  return snapshot.files.find(
    (item) => item.scope === change.scope && item.path === change.path,
  );
}

function bindChanges(
  snapshot: AssistantAuthoringSnapshot,
  changes: readonly AssistantFileChange[],
  refreshed?: AssistantAuthoringSnapshot,
): BoundChange[] {
  return changes.map((change) => {
    const creation = "create" in change;
    const file = creation
      ? findSnapshotFile(refreshed ?? { ...snapshot, files: [] }, change) ?? findSnapshotFile(snapshot, change)
      : findSnapshotFile(snapshot, change) ?? findSnapshotFile(refreshed ?? { ...snapshot, files: [] }, change);
    if (creation) {
      if (!isLocalProjectEditorPath(snapshot.editor_path)) {
        throw new Error("File creation is only available for a local project editor.");
      }
      if (
        !file?.is_manifest_declared ||
        file.available ||
        file.reason !== "declared_missing_local_file"
      ) {
        throw new Error(
          `${change.scope}:${change.path} is not an explicitly declared missing local authoring file.`,
        );
      }
      return { ...change };
    }
    if (!file?.available || !file.sha256) {
      throw new Error(
        `${change.scope}:${change.path} is not available in the current authoring snapshot.`,
      );
    }
    return { ...change, expected_sha256: file.sha256 };
  });
}

const refreshedSnapshots = new WeakMap<
  AssistantAuthoringSnapshot,
  AssistantAuthoringSnapshot
>();

async function resolveRefresh(
  snapshot: AssistantAuthoringSnapshot,
  changes: readonly AssistantFileChange[],
): Promise<AssistantAuthoringSnapshot | undefined> {
  const includesCreation = changes.some((change) => "create" in change);
  const needsRefresh = changes.some(
    (change) => "create" in change || !findSnapshotFile(snapshot, change),
  );
  if (!needsRefresh || !isLocalProjectEditorPath(snapshot.editor_path)) return undefined;
  const cached = includesCreation ? undefined : refreshedSnapshots.get(snapshot);
  if (cached) return cached;
  const refreshed = await snapshotAssistantAuthoring(snapshot.editor_path);
  if (refreshed.editor_path !== snapshot.editor_path) {
    throw new Error("The authoring snapshot changed editor path while refreshing.");
  }
  if (!includesCreation) refreshedSnapshots.set(snapshot, refreshed);
  return refreshed;
}

function authoringRequest(
  endpoint: "preview" | "commit",
  snapshot: AssistantAuthoringSnapshot,
  changes: readonly AssistantFileChange[],
): Promise<AssistantAuthoringResult> {
  return resolveRefresh(snapshot, changes).then((refreshed) =>
    apiRequest(`${BASE}/${endpoint}`, {
      method: "POST",
      body: JSON.stringify({
        editor_path: snapshot.editor_path,
        version: snapshot.version,
        changes: bindChanges(snapshot, changes, refreshed),
      }),
    }),
  );
}

export function previewAssistantAuthoring(
  snapshot: AssistantAuthoringSnapshot,
  changes: readonly AssistantFileChange[],
): Promise<AssistantAuthoringResult> {
  return authoringRequest("preview", snapshot, changes);
}

export function commitAssistantAuthoring(
  snapshot: AssistantAuthoringSnapshot,
  changes: readonly AssistantFileChange[],
): Promise<AssistantAuthoringResult> {
  return authoringRequest("commit", snapshot, changes);
}

/**
 * Commit a selected subset of the active manifest's declared files. The
 * caller supplies only relative paths and a message; scopes and optimistic
 * hashes are rebound from this current host snapshot before they cross HTTP.
 */
export function commitAssistantAuthoringGit(
  snapshot: AssistantAuthoringSnapshot,
  paths: readonly string[],
  message: string,
): Promise<AssistantAuthoringGitCommitResult> {
  const files = bindGitCommitFiles(snapshot, paths);
  return apiRequest(`${BASE}/git-commit`, {
    method: "POST",
    body: JSON.stringify({
      editor_path: snapshot.editor_path,
      files,
      message,
    }),
  });
}

export function publishAssistantAuthoringGit(
  snapshot: AssistantAuthoringSnapshot,
  commit: string,
  branch: string,
): Promise<AssistantAuthoringGitPublishResult> {
  return apiRequest(`${BASE}/git-push`, {
    method: "POST",
    body: JSON.stringify({ editor_path: snapshot.editor_path, commit, branch }),
  });
}

/**
 * Rebind a model spelling to one and only one snapshot entry. A conversation
 * often names a bundle file from the workspace root (`bots/name/main.bot`),
 * while `authoring.files` deliberately exposes it bundle-relative
 * (`main.bot`). Accept that single, host-derived alternate spelling only for
 * bundle entries below the editor's own bundle directory. Never use a basename
 * or arbitrary suffix match: those could silently bind another declared file.
 */
export function bindGitCommitFiles(
  snapshot: AssistantAuthoringSnapshot,
  paths: readonly string[],
): Array<{ scope: "bundle" | "workspace"; path: string; expected_sha256: string }> {
  const slash = snapshot.editor_path.lastIndexOf("/");
  const bundleDir = slash > 0 ? snapshot.editor_path.slice(0, slash) : "";
  return paths.map((requestedPath) => {
    const candidates = snapshot.files.filter((file) =>
      file.path === requestedPath ||
      (file.scope === "bundle" &&
        bundleDir !== "" &&
        `${bundleDir}/${file.path}` === requestedPath),
    );
    const committable = candidates.filter(
      (file) => file.git_committable !== false,
    );
    const file = committable[0];
    if (!file || committable.length !== 1) {
      if (candidates.length > 0 && committable.length === 0) {
        throw new Error(
          `${requestedPath} is replaceable in the active editor but is not manifest-declared for Git commit.`,
        );
      }
      throw new Error(
        `${requestedPath} does not identify exactly one Git-committable file in the current authoring snapshot.`,
      );
    }
    if (!file.available || !file.sha256) {
      throw new Error(`${requestedPath} is not available in the current authoring snapshot.`);
    }
    return {
      scope: file.scope,
      path: file.path,
      expected_sha256: file.sha256,
    };
  });
}
