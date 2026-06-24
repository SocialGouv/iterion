// Native kanban tracker — REST client. Mirrors pkg/dispatcher/native/http.go.
// All paths are relative to the studio's same-origin server.

import { apiRequest } from "./client";

const BASE = "/api/v1/native";

function request<T>(path: string, init?: RequestInit): Promise<T> {
  return apiRequest<T>(BASE + path, init);
}

// ---------------------------------------------------------------------------
// Types — mirror pkg/dispatcher/native/*.go JSON tags
// ---------------------------------------------------------------------------

export interface NativeIssue {
  id: string;
  title: string;
  body?: string;
  state: string;
  labels?: string[];
  priority?: number;
  assignee?: string;
  blockers?: string[];
  fields?: Record<string, unknown>;
  /** Per-ticket bot name (overrides the dispatcher's per-assignee /
   *  global workflow selection at launch time). */
  bot?: string;
  /** Per-ticket workflow var overrides. String-valued to match the
   *  studio's Launch form wire format — engine handles coercion. */
  bot_args?: Record<string, string>;
  claim?: string;
  /** ID of the most recent dispatcher-spawned run that processed this
   *  issue. Stamped by the dispatcher on every finish (success or
   *  failure). Empty for issues never picked up by a dispatcher. */
  last_run_id?: string;
  /** Absolute filesystem path the last run executed in — typically the
   *  worktree directory when `worktree: auto` was used, otherwise the
   *  per-issue dispatcher workspace. Surfaced in the IssueModal as a
   *  copy/vscode link so operators can inspect the diff manually. */
  last_workdir?: string;
  created_at: string;
  updated_at: string;
}

export interface NativeBoard {
  states: NativeState[];
  fields?: NativeField[];
  updated_at: string;
}

export interface NativeState {
  name: string;
  display?: string;
  color?: string;
  terminal?: boolean;
  eligible?: boolean;
}

export type NativeFieldType = "text" | "number" | "enum" | "date" | "bool";

export interface NativeField {
  name: string;
  display?: string;
  type: NativeFieldType;
  required?: boolean;
  enum_values?: string[];
  default?: unknown;
}

export interface NativeIssueCreate {
  title: string;
  body?: string;
  state?: string;
  labels?: string[];
  priority?: number;
  assignee?: string;
  blockers?: string[];
  fields?: Record<string, unknown>;
  bot?: string;
  bot_args?: Record<string, string>;
}

export interface NativeIssuePatch {
  title?: string;
  body?: string;
  labels?: string[];
  priority?: number;
  assignee?: string;
  blockers?: string[];
  fields?: Record<string, unknown>;
  bot?: string;
  bot_args?: Record<string, string>;
}

// ---------------------------------------------------------------------------
// REST surface
// ---------------------------------------------------------------------------

export interface ListFilter {
  state?: string[];
  label?: string[];
  assignee?: string;
}

export function listIssues(filter: ListFilter = {}): Promise<NativeIssue[]> {
  const q = new URLSearchParams();
  for (const s of filter.state ?? []) q.append("state", s);
  for (const l of filter.label ?? []) q.append("label", l);
  if (filter.assignee) q.set("assignee", filter.assignee);
  const suffix = q.toString();
  return request(`/issues${suffix ? "?" + suffix : ""}`);
}

export function createIssue(input: NativeIssueCreate): Promise<NativeIssue> {
  return request("/issues", { method: "POST", body: JSON.stringify(input) });
}

export function getIssue(id: string): Promise<NativeIssue> {
  return request(`/issues/${encodeURIComponent(id)}`);
}

export function patchIssue(id: string, patch: NativeIssuePatch): Promise<NativeIssue> {
  return request(`/issues/${encodeURIComponent(id)}`, {
    method: "PATCH",
    body: JSON.stringify(patch),
  });
}

export function deleteIssue(id: string): Promise<void> {
  return request(`/issues/${encodeURIComponent(id)}`, { method: "DELETE" });
}

export function transitionIssue(id: string, to: string): Promise<NativeIssue> {
  return request(`/issues/${encodeURIComponent(id)}/transition`, {
    method: "POST",
    body: JSON.stringify({ to }),
  });
}

export function getBoard(): Promise<NativeBoard> {
  return request("/board");
}

export function putBoard(board: Partial<NativeBoard>): Promise<NativeBoard> {
  return request("/board", { method: "PUT", body: JSON.stringify(board) });
}

// ---------------------------------------------------------------------------
// Column (state) management. Mirrors the native /board/states REST surface.
// Each call returns the refreshed board so callers refresh without a
// second getBoard(). Rename/delete cascade across issues server-side.
// ---------------------------------------------------------------------------

// Editable per-column fields. `name` triggers a cascading rename when it
// differs from the path segment.
export type NativeStatePatch = Partial<
  Pick<NativeState, "name" | "display" | "color" | "eligible" | "terminal">
>;

export function addState(state: NativeState): Promise<NativeBoard> {
  return request("/board/states", { method: "POST", body: JSON.stringify(state) });
}

export function updateState(name: string, patch: NativeStatePatch): Promise<NativeBoard> {
  return request(`/board/states/${encodeURIComponent(name)}`, {
    method: "PATCH",
    body: JSON.stringify(patch),
  });
}

// deleteState removes a column. When the column is non-empty and no
// migrateTo is given, the server returns 409 (ApiError) so the caller can
// prompt for a destination column.
export function deleteState(name: string, migrateTo?: string): Promise<NativeBoard> {
  const q = migrateTo ? `?migrate_to=${encodeURIComponent(migrateTo)}` : "";
  return request(`/board/states/${encodeURIComponent(name)}${q}`, { method: "DELETE" });
}

export function reorderStates(order: string[]): Promise<NativeBoard> {
  return request("/board/states/reorder", {
    method: "POST",
    body: JSON.stringify({ order }),
  });
}

// ---------------------------------------------------------------------------
// Label vocabulary management. Mirrors the native /labels REST surface.
// ---------------------------------------------------------------------------

export interface LabelUsage {
  label: string;
  count: number;
  last_used_at?: string;
}

export interface LabelOpResult {
  touched: number;
}

export function listLabels(): Promise<LabelUsage[]> {
  return request("/labels");
}

export function renameLabel(from: string, to: string): Promise<LabelOpResult> {
  return request("/labels/rename", {
    method: "POST",
    body: JSON.stringify({ from, to }),
  });
}

export function mergeLabels(from: string, to: string): Promise<LabelOpResult> {
  return request("/labels/merge", {
    method: "POST",
    body: JSON.stringify({ from, to }),
  });
}

export function deleteLabel(label: string): Promise<LabelOpResult> {
  return request(`/labels/${encodeURIComponent(label)}`, { method: "DELETE" });
}
