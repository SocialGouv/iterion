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
  /** Explicit cloud attachment enrichment; absent from the server snapshot. */
  complete?: boolean;
  source?: string;
  reason?: string;
}

export interface AssistantAuthoringSnapshot {
  editor_path: string;
  version?: number;
  files: AssistantAuthoringFileSnapshot[];
}

export interface AssistantFileReplacement {
  before: string;
  after: string;
}

export interface AssistantFileChange {
  scope: "bundle" | "workspace";
  path: string;
  replacements: AssistantFileReplacement[];
}

export interface AssistantAuthoringPreviewFile {
  scope: "bundle" | "workspace";
  path: string;
  before: string;
  after: string;
}

export interface AssistantAuthoringResult {
  files: AssistantAuthoringPreviewFile[];
  version?: number;
  saved: boolean;
}

interface BoundChange extends AssistantFileChange {
  expected_sha256: string;
}

export function snapshotAssistantAuthoring(
  editorPath: string,
): Promise<AssistantAuthoringSnapshot> {
  return apiRequest(`${BASE}/snapshot`, {
    method: "POST",
    body: JSON.stringify({ editor_path: editorPath }),
  });
}

function bindChanges(
  snapshot: AssistantAuthoringSnapshot,
  changes: readonly AssistantFileChange[],
): BoundChange[] {
  return changes.map((change) => {
    const file = snapshot.files.find(
      (item) => item.scope === change.scope && item.path === change.path,
    );
    if (!file?.available || !file.sha256) {
      throw new Error(
        `${change.scope}:${change.path} is not available in the captured authoring snapshot.`,
      );
    }
    return { ...change, expected_sha256: file.sha256 };
  });
}

function authoringRequest(
  endpoint: "preview" | "commit",
  snapshot: AssistantAuthoringSnapshot,
  changes: readonly AssistantFileChange[],
): Promise<AssistantAuthoringResult> {
  return apiRequest(`${BASE}/${endpoint}`, {
    method: "POST",
    body: JSON.stringify({
      editor_path: snapshot.editor_path,
      version: snapshot.version,
      changes: bindChanges(snapshot, changes),
    }),
  });
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
