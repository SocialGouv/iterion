// Pipeline board — read-model client for the single, global, runtime-derived
// board. Distinct from api/native.ts: /board remains the editable backlog,
// while this API projects every root pipeline (and not-yet-launched native
// task) into four fixed, non-draggable lanes. Card position is derived from
// server state only — there is no drag-and-drop here.

import { apiRequest } from "./client";
import type { NativeIssue } from "./native";

const BASE = "/api/v1/pipeline-board";

// The four fixed lanes, in the order the server emits them.
export interface PipelineBoardColumn {
  id: string; // "todo" | "in_progress" | "done" | "attention"
  title: string;
  kind: string;
}

// One paused human interaction somewhere in a root's tree (the root itself or
// any descendant). The IN_PROGRESS card presents these one at a time; each
// answer targets the exact run_id shown here.
export interface PipelineBoardPendingReview {
  run_id: string;
  workflow_name?: string;
  bot_id?: string;
  node_id?: string;
  interaction_id?: string;
  questions?: Record<string, unknown>;
  depth: number;
}

// One dispatcher attempt associated with a native task-backed root.
export interface PipelineBoardAttempt {
  run_id?: string;
  status?: string;
  at?: string;
}

// PipelineBoardCard is the read model the studio polls: one per root pipeline
// (or per not-yet-launched native task). Descendants are folded into their
// root — there are no per-child cards.
export interface PipelineBoardCard {
  id: string;
  kind: "task" | "run" | string;
  column_id: string;
  title: string;
  body?: string;

  // Native task provenance (present when the root is backed by a board issue).
  issue_id?: string;
  issue_state?: string;
  labels?: string[];
  priority?: number;

  // Run identity (empty for a not-yet-launched task card).
  run_id?: string;
  workflow_name?: string;
  bot_id?: string;
  status?: string;
  error?: string;

  // TODO lane — launch vars / task bot-args, and the concurrency-queue place.
  entry_input?: Record<string, unknown>;
  queue_position?: number;

  // IN_PROGRESS lane — node progress for the root and the whole tree.
  executed_nodes: number;
  total_nodes: number;
  tree_executed_nodes: number;
  tree_total_nodes: number;
  descendant_count?: number;
  pending_reviews?: PipelineBoardPendingReview[];

  // DONE lane — the pipeline's output (final_answer, else latest artifact).
  output?: string;

  attempts?: PipelineBoardAttempt[];
  created_at: string;
  updated_at: string;
}

// The local pipeline-concurrency gate. When `enabled` is false the other
// fields are zero and the TODO lane only holds not-yet-launched native tasks.
export interface PipelineConcurrency {
  enabled: boolean;
  max: number;
  active: number;
  waiting: number;
}

// PipelineBoard is the aggregate global read model returned by the board GET.
export interface PipelineBoard {
  columns: PipelineBoardColumn[];
  cards: PipelineBoardCard[];
  concurrency: PipelineConcurrency;
  generated_at?: string;
  topology_error?: string;
}

export interface CreatePipelineTaskInput {
  // The bot the created task runs as. Required — the board is global, so the
  // bot comes from the request body (not a URL path).
  bot: string;
  title: string;
  body?: string;
  labels?: string[];
  priority?: number;
  bot_args?: Record<string, string>;
  start?: boolean;
}

type UnknownRecord = Record<string, unknown>;

function record(value: unknown): UnknownRecord | null {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? (value as UnknownRecord)
    : null;
}

function text(value: unknown): string | undefined {
  return typeof value === "string" && value.trim() ? value : undefined;
}

function numberValue(value: unknown): number | undefined {
  return typeof value === "number" && Number.isFinite(value) ? value : undefined;
}

function intValue(value: unknown, fallback = 0): number {
  const n = numberValue(value);
  return n === undefined ? fallback : Math.trunc(n);
}

function booleanValue(value: unknown): boolean {
  return value === true;
}

function stringArray(value: unknown): string[] | undefined {
  if (!Array.isArray(value)) return undefined;
  return value.filter((entry): entry is string => typeof entry === "string");
}

function normalizeAttempts(value: unknown): PipelineBoardAttempt[] | undefined {
  if (!Array.isArray(value)) return undefined;
  return value.map((entry) => {
    const source = record(entry) ?? {};
    return {
      ...(text(source.run_id) ? { run_id: text(source.run_id) } : {}),
      ...(text(source.status) ? { status: text(source.status) } : {}),
      ...(text(source.at) ? { at: text(source.at) } : {}),
    };
  });
}

function normalizePendingReviews(
  value: unknown,
): PipelineBoardPendingReview[] | undefined {
  if (!Array.isArray(value)) return undefined;
  return value.map((entry) => {
    const source = record(entry) ?? {};
    const questions = record(source.questions) ?? undefined;
    return {
      run_id: text(source.run_id) ?? "",
      ...(text(source.workflow_name)
        ? { workflow_name: text(source.workflow_name) }
        : {}),
      ...(text(source.bot_id) ? { bot_id: text(source.bot_id) } : {}),
      ...(text(source.node_id) ? { node_id: text(source.node_id) } : {}),
      ...(text(source.interaction_id)
        ? { interaction_id: text(source.interaction_id) }
        : {}),
      ...(questions ? { questions } : {}),
      depth: Math.max(0, intValue(source.depth, 0)),
    };
  });
}

export function normalizePipelineBoardColumn(
  value: unknown,
  index = 0,
): PipelineBoardColumn {
  const source = record(value) ?? {};
  const id = text(source.id) ?? `column-${index + 1}`;
  return {
    id,
    title: text(source.title) ?? id,
    kind: text(source.kind) ?? id,
  };
}

export function normalizePipelineBoardCard(
  value: unknown,
  index = 0,
): PipelineBoardCard {
  const source = record(value) ?? {};
  const runID = text(source.run_id);
  const issueID = text(source.issue_id);
  const id = text(source.id) ?? runID ?? issueID ?? `card-${index + 1}`;
  const entryInput = record(source.entry_input) ?? undefined;
  const attempts = normalizeAttempts(source.attempts);
  const reviews = normalizePendingReviews(source.pending_reviews);
  return {
    id,
    kind: text(source.kind) ?? (runID ? "run" : "task"),
    column_id: text(source.column_id) ?? "todo",
    title: text(source.title) ?? id,
    ...(text(source.body) ? { body: text(source.body) } : {}),
    ...(issueID ? { issue_id: issueID } : {}),
    ...(text(source.issue_state) ? { issue_state: text(source.issue_state) } : {}),
    ...(stringArray(source.labels) !== undefined
      ? { labels: stringArray(source.labels) }
      : {}),
    ...(numberValue(source.priority) !== undefined
      ? { priority: numberValue(source.priority) }
      : {}),
    ...(runID ? { run_id: runID } : {}),
    ...(text(source.workflow_name)
      ? { workflow_name: text(source.workflow_name) }
      : {}),
    ...(text(source.bot_id) ? { bot_id: text(source.bot_id) } : {}),
    ...(text(source.status) ? { status: text(source.status) } : {}),
    ...(text(source.error) ? { error: text(source.error) } : {}),
    ...(entryInput ? { entry_input: entryInput } : {}),
    ...(numberValue(source.queue_position) !== undefined
      ? { queue_position: numberValue(source.queue_position) }
      : {}),
    executed_nodes: intValue(source.executed_nodes, 0),
    total_nodes: intValue(source.total_nodes, 0),
    tree_executed_nodes: intValue(source.tree_executed_nodes, 0),
    tree_total_nodes: intValue(source.tree_total_nodes, 0),
    ...(numberValue(source.descendant_count) !== undefined
      ? { descendant_count: numberValue(source.descendant_count) }
      : {}),
    ...(reviews !== undefined ? { pending_reviews: reviews } : {}),
    ...(text(source.output) ? { output: text(source.output) } : {}),
    ...(attempts !== undefined ? { attempts } : {}),
    created_at: text(source.created_at) ?? "",
    updated_at: text(source.updated_at) ?? "",
  };
}

function normalizeConcurrency(value: unknown): PipelineConcurrency {
  const source = record(value) ?? {};
  return {
    enabled: booleanValue(source.enabled),
    max: intValue(source.max, 0),
    active: intValue(source.active, 0),
    waiting: intValue(source.waiting, 0),
  };
}

// Keep all wire-shape tolerance in this module. Views always receive one
// canonical board with the four fixed columns and folded root cards.
export function normalizePipelineBoard(value: unknown): PipelineBoard {
  const root = record(value) ?? {};
  const columns = Array.isArray(root.columns) ? root.columns : [];
  const cards = Array.isArray(root.cards) ? root.cards : [];
  return {
    columns: columns.map(normalizePipelineBoardColumn),
    cards: cards.map(normalizePipelineBoardCard),
    concurrency: normalizeConcurrency(root.concurrency),
    ...(text(root.generated_at) ? { generated_at: text(root.generated_at) } : {}),
    ...(text(root.topology_error)
      ? { topology_error: text(root.topology_error) }
      : {}),
  };
}

export async function getPipelineBoard(opts?: {
  signal?: AbortSignal;
}): Promise<PipelineBoard> {
  const raw = await apiRequest<unknown>(BASE, { signal: opts?.signal });
  return normalizePipelineBoard(raw);
}

export async function createPipelineTask(
  input: CreatePipelineTaskInput,
): Promise<NativeIssue> {
  return apiRequest<NativeIssue>(`${BASE}/tasks`, {
    method: "POST",
    body: JSON.stringify(input),
  });
}
