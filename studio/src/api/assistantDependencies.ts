import { apiRequest } from "./client";
import type { AssistantAuthoringSnapshot } from "./assistantAuthoring";

const BASE = "/api/v1/assistant/dependencies";

export interface AssistantDependencyBotsUpdateResult {
  name: string;
  previous_ref: string;
  ref: string;
  bundle_sha256: string;
  commit: string;
  installed_path: string;
}

export interface AssistantDependencyBotsLocalizeResult {
  name: string;
  local_source: string;
  source_commit: string;
  lock_commit: string;
  bundle_sha256: string;
  installed_path: string;
  resumed_after_import: boolean;
}

// The host owns the project, source, materialization directory and Git
// command. Copi can only request one existing dependency at an immutable SHA.
export function updateAssistantBotDependency(
  name: string,
  ref: string,
  message: string,
): Promise<AssistantDependencyBotsUpdateResult> {
  return apiRequest(`${BASE}/bots-update`, {
    method: "POST",
    body: JSON.stringify({ name, ref, message }),
  });
}

// The active authoring snapshot binds localization to the currently open
// consumer bundle. Copi supplies only the existing dependency name and the
// source-import commit message; it never chooses filesystem paths or Git args.
export function localizeAssistantBotDependency(
  snapshot: AssistantAuthoringSnapshot,
  name: string,
  message: string,
): Promise<AssistantDependencyBotsLocalizeResult> {
  return apiRequest(`${BASE}/bots-localize`, {
    method: "POST",
    body: JSON.stringify({ editor_path: snapshot.editor_path, name, message }),
  });
}
