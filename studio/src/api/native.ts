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
  /** Denormalized best-effort HINT that the issue's most recent run parked
   *  awaiting human/operator input. Lets the board grid render a per-card
   *  "⏸ Awaiting input" badge WITHOUT an N+1 run fetch. Not authoritative —
   *  the IssueModal's answer affordance still keys off the run status; a
   *  stale flag is corrected at the next card touch. */
  awaiting_input?: boolean;
  /** Append-only run history (mirrors Go's `runs`), newest-last, deduped by
   *  run_id. The full history the Ticket tab renders as a list;
   *  last_run_id/last_workdir remain the single-pointer back-compat view.
   *  Absent on records written before run history was tracked. */
  runs?: RunRef[];
  /** Typed forge linkage (mirrors Go's `external`). Present once a card is
   *  pushed to / synced from a forge. This is the canonical source of truth
   *  for the card's PR/CI panel + push semantics (replaces the legacy
   *  fields["forge:*"] shape). */
  external?: ExternalLink;
  created_at: string;
  updated_at: string;
}

// RunRef is one entry in an issue's run history (mirrors Go's native.RunRef).
export interface RunRef {
  run_id: string;
  workdir?: string;
  at: string;
}

// ExternalLink is a card's typed link to a forge issue/PR. `url` and `state`
// are populated by the server once the linked issue exists.
export interface ExternalLink {
  provider: string;
  connection_id: string;
  repo: string;
  number: number;
  url?: string;
  state?: string;
  // Forge login that opened the linked issue (identity behind the
  // author-trust gate; shown on parked needs:approval cards).
  author?: string;
}

// ExternalLinkInput is the WRITE shape for repo-first scoping: operator
// picks a connected repo (provider + connection_id + repo_full_name) and
// the server fills number/url/state via push-to-forge or the sync worker.
export type ExternalLinkInput = Pick<
  ExternalLink,
  "provider" | "connection_id" | "repo"
> &
  Partial<Pick<ExternalLink, "number" | "url" | "state">>;

export interface NativeBoard {
  states: NativeState[];
  fields?: NativeField[];
  views?: NativeView[];
  updated_at: string;
}

// NativeView mirrors pkg/dispatcher/native.View — a saved filter/sort/group
// preset persisted in board.json and shared across operators.
export interface NativeView {
  name: string;
  search?: string;
  labels?: string[];
  assignee?: string;
  // bot scopes the view to a single bot (NativeIssue.bot) — a persisted
  // filter for saving "the X pipeline" lens. Additive to group_by="bot".
  bot?: string;
  sort?: string;
  group_by?: string;
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
  // Optional repo-first scoping: link the new card to a connected forge
  // repo at creation. number/url/state stay empty until push-to-forge or
  // the sync worker populate them.
  external?: ExternalLinkInput;
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
  // When present, re-links the card's forge repo (absent = unchanged; the
  // server keeps sync-owned number/url/state semantics).
  external?: ExternalLinkInput;
}

// ---------------------------------------------------------------------------
// Forge linkage — push a card to a forge, and read the forge's linked PRs +
// CI status back. Cloud-mode only; mirror pkg/server's native forge routes.
// ---------------------------------------------------------------------------

// PushIssueBody is omitted for a card already linked to a forge (its
// fields["forge:url"] is set — the server updates the linked issue). For an
// UNLINKED card both connection_id and repo ("owner/repo") are required.
export interface PushIssueBody {
  connection_id?: string;
  repo?: string;
}

export interface PushIssueResult {
  url: string;
  number: number;
  provider: string;
}

// PullRef mirrors a forge pull request (merge request on GitLab) linked to
// the issue.
export interface PullRef {
  number: number;
  title: string;
  state: string;
  url: string;
  source_branch: string;
  target_branch: string;
  head_sha: string;
  draft: boolean;
  author: string;
  linked_issues: string[];
}

// CIRun is a single check/pipeline run on a PR's head commit.
export interface CIRun {
  name: string;
  status: string;
  conclusion: string;
  url: string;
  sha: string;
  started_at: string;
  finished_at: string;
}

// CIStatus is the aggregate CI state for a PR's head commit plus the current
// runs; `history` (returned alongside it) carries recent prior runs.
export interface CIStatus {
  sha: string;
  state: string;
  runs: CIRun[];
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

export function commentIssue(
  id: string,
  body: string,
): Promise<NativeIssue> {
  return request(`/issues/${encodeURIComponent(id)}/comments`, {
    method: "POST",
    body: JSON.stringify({ body }),
  });
}

// pushIssueToForge mirrors a card to its forge. Omit `body` for a card
// already linked to a forge (server updates the linked issue); pass
// {connection_id, repo} to create-and-link an unlinked card.
export function pushIssueToForge(id: string, body?: PushIssueBody): Promise<PushIssueResult> {
  return request(`/issues/${encodeURIComponent(id)}/push`, {
    method: "POST",
    body: JSON.stringify(body ?? {}),
  });
}

// listIssuePulls returns the forge pull requests (merge requests on GitLab)
// linked to a card.
export function listIssuePulls(id: string): Promise<PullRef[]> {
  return request<{ pulls: PullRef[] }>(`/issues/${encodeURIComponent(id)}/pulls`).then(
    (r) => r.pulls ?? [],
  );
}

// CreateIssuePullBody opens a PR linking the card to a forge. A forge-linked
// card reuses its connection + repo (omit connection_id/repo); an unlinked card
// must supply both connection_id and repo ("owner/repo"). title/body default
// server-side from the card when omitted.
export interface CreateIssuePullBody {
  connection_id?: string;
  repo?: string;
  title?: string;
  body?: string;
  source_branch: string;
  target_branch: string;
  draft?: boolean;
}

// createIssuePull opens a forge pull request (merge request on GitLab) for the
// card and returns the new PR ref.
export function createIssuePull(id: string, body: CreateIssuePullBody): Promise<PullRef> {
  return request(`/issues/${encodeURIComponent(id)}/pulls`, {
    method: "POST",
    body: JSON.stringify(body),
  });
}

export type MergeMethod = "merge" | "squash" | "rebase";

// MergeIssuePullBody controls how a linked PR is merged. All fields optional —
// the forge applies its defaults when omitted.
export interface MergeIssuePullBody {
  method?: MergeMethod;
  commit_title?: string;
  commit_message?: string;
  delete_branch?: boolean;
}

// mergeIssuePull merges a linked PR and returns the merged PR ref.
export function mergeIssuePull(
  id: string,
  number: number,
  body: MergeIssuePullBody = {},
): Promise<PullRef> {
  return request(`/issues/${encodeURIComponent(id)}/pulls/${number}/merge`, {
    method: "POST",
    body: JSON.stringify(body),
  });
}

// getIssuePullCI returns the aggregate CI status + recent run history for a
// linked PR's head commit.
export function getIssuePullCI(
  id: string,
  number: number,
): Promise<{ status: CIStatus; history: CIRun[] }> {
  return request(`/issues/${encodeURIComponent(id)}/pulls/${number}/ci`);
}

export function getBoard(): Promise<NativeBoard> {
  return request("/board");
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
// Custom field schema management. Mirrors the native /board/fields REST
// surface. Rename cascades the key across issue.Fields; delete strips it.
// ---------------------------------------------------------------------------

export type NativeFieldPatch = Partial<
  Pick<NativeField, "name" | "display" | "type" | "required" | "enum_values">
>;

export function addField(field: NativeField): Promise<NativeBoard> {
  return request("/board/fields", { method: "POST", body: JSON.stringify(field) });
}

export function updateField(name: string, patch: NativeFieldPatch): Promise<NativeBoard> {
  return request(`/board/fields/${encodeURIComponent(name)}`, {
    method: "PATCH",
    body: JSON.stringify(patch),
  });
}

export function deleteField(name: string): Promise<NativeBoard> {
  return request(`/board/fields/${encodeURIComponent(name)}`, { method: "DELETE" });
}

export function reorderFields(order: string[]): Promise<NativeBoard> {
  return request("/board/fields/reorder", {
    method: "POST",
    body: JSON.stringify({ order }),
  });
}

// ---------------------------------------------------------------------------
// Saved views. Mirrors the native /board/views REST surface. saveView
// upserts by name; both return the refreshed board.
// ---------------------------------------------------------------------------

export function saveView(view: NativeView): Promise<NativeBoard> {
  return request("/board/views", { method: "POST", body: JSON.stringify(view) });
}

export function deleteView(name: string): Promise<NativeBoard> {
  return request(`/board/views/${encodeURIComponent(name)}`, { method: "DELETE" });
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
