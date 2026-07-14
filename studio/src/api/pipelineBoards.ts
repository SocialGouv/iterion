// Pipeline Boards — read-model client for the bot-scoped, runtime-derived
// board. This is deliberately separate from api/native.ts: /board remains
// the editable backlog, while this API projects one bot's pipeline topology
// and current run tree into non-draggable columns.

import { apiRequest } from "./client";
import type { NativeIssue } from "./native";

const BASE = "/api/v1/pipeline-boards";

export interface PipelineBoardIdentity {
  id: string;
  bot_id: string;
  display_name: string;
  icon?: string;
  description?: string;
  enabled: boolean;
}

export interface PipelineBoardColumn {
  id: string;
  title: string;
  kind: string;
  workflow_name?: string;
  node_id?: string;
  interaction_mode?: string;
}

export interface PipelineBoardAttempt {
  run_id?: string;
  status?: string;
  at?: string;
}

export interface PipelineBoardCard {
  id: string;
  kind: "task" | "run" | string;
  column_id: string;
  title: string;
  body?: string;
  issue_id?: string;
  issue_state?: string;
  labels?: string[];
  priority?: number;
  run_id?: string;
  root_run_id?: string;
  parent_run_id?: string;
  depth: number;
  workflow_name?: string;
  bot_id?: string;
  status?: string;
  error?: string;
  node_id?: string;
  interaction_id?: string;
  questions?: Record<string, unknown>;
  created_at?: string;
  updated_at?: string;
  attempts?: PipelineBoardAttempt[];
  children_count?: number;
}

export interface PipelineBoardDetail {
  board: PipelineBoardIdentity;
  columns: PipelineBoardColumn[];
  cards: PipelineBoardCard[];
  generated_at?: string;
  topology_error?: string;
}

export interface PipelineBoardListItem {
  board: PipelineBoardIdentity;
  column_count?: number;
  card_count?: number;
  awaiting_input_count?: number;
  generated_at?: string;
  topology_error?: string;
}

export interface CreatePipelineTaskInput {
  title: string;
  body?: string;
  labels?: string[];
  priority?: number;
  bot_args?: Record<string, string>;
  start: boolean;
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

function booleanValue(value: unknown): boolean | undefined {
  return typeof value === "boolean" ? value : undefined;
}

function stringArray(value: unknown): string[] | undefined {
  if (!Array.isArray(value)) return undefined;
  const out = value.filter((entry): entry is string => typeof entry === "string");
  return out.length > 0 ? out : [];
}

function normalizeAttempts(value: unknown): PipelineBoardAttempt[] | undefined {
  if (Array.isArray(value)) {
    return value.map((entry) => {
      const source = record(entry) ?? {};
      return {
        ...(text(source.run_id) ? { run_id: text(source.run_id) } : {}),
        ...(text(source.status) ? { status: text(source.status) } : {}),
        ...(text(source.at) ? { at: text(source.at) } : {}),
      };
    });
  }
  // A few early prototypes returned only the count. Preserve that count in
  // the canonical array shape so rendering can consistently use `.length`.
  const legacyCount = numberValue(value);
  if (legacyCount === undefined) return undefined;
  return Array.from({ length: Math.max(0, Math.trunc(legacyCount)) }, () => ({}));
}

function normalizeIdentity(value: unknown): PipelineBoardIdentity {
  const source = record(value) ?? {};
  // bot/name aliases keep the client tolerant of early backend revisions;
  // the rest of Studio only consumes the canonical shape below.
  const botID =
    text(source.bot_id) ?? text(source.bot) ?? text(source.name) ?? text(source.id) ?? "";
  const id = text(source.id) ?? botID;
  return {
    id,
    bot_id: botID,
    display_name:
      text(source.display_name) ?? text(source.title) ?? text(source.name) ?? botID,
    ...(text(source.icon) ? { icon: text(source.icon) } : {}),
    ...(text(source.description) ? { description: text(source.description) } : {}),
    enabled: booleanValue(source.enabled) ?? true,
  };
}

function normalizeColumn(value: unknown, index: number): PipelineBoardColumn {
  const source = record(value) ?? {};
  const nodeID = text(source.node_id);
  const id = text(source.id) ?? nodeID ?? `column-${index + 1}`;
  return {
    id,
    title: text(source.title) ?? text(source.display_name) ?? text(source.name) ?? id,
    kind: text(source.kind) ?? (nodeID ? "interaction" : "state"),
    ...(text(source.workflow_name)
      ? { workflow_name: text(source.workflow_name) }
      : {}),
    ...(nodeID ? { node_id: nodeID } : {}),
    ...(text(source.interaction_mode)
      ? { interaction_mode: text(source.interaction_mode) }
      : {}),
  };
}

export function normalizePipelineBoardCard(
  value: unknown,
  index = 0,
): PipelineBoardCard {
  const source = record(value) ?? {};
  const checkpoint = record(source.checkpoint);
  const issueID = text(source.issue_id) ?? text(source.issue);
  const runID = text(source.run_id) ?? text(source.run);
  const nodeID = text(source.node_id) ?? text(checkpoint?.node_id);
  const questions =
    record(source.questions) ??
    record(checkpoint?.questions) ??
    record(checkpoint?.interaction_questions) ??
    undefined;
  const attempts = normalizeAttempts(source.attempts);
  const id = text(source.id) ?? runID ?? issueID ?? `card-${index + 1}`;
  const depth = Math.max(0, Math.trunc(numberValue(source.depth) ?? 0));
  return {
    id,
    kind: text(source.kind) ?? (runID ? "run" : "task"),
    column_id:
      text(source.column_id) ?? text(source.column) ?? text(source.state) ?? "unmapped",
    title:
      text(source.title) ?? text(source.issue_title) ?? text(source.workflow_name) ?? id,
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
    ...(text(source.root_run_id) ? { root_run_id: text(source.root_run_id) } : {}),
    ...(text(source.parent_run_id) ?? text(source.parent_id)
      ? { parent_run_id: text(source.parent_run_id) ?? text(source.parent_id) }
      : {}),
    depth,
    ...(text(source.workflow_name)
      ? { workflow_name: text(source.workflow_name) }
      : {}),
    ...(text(source.bot_id) ?? text(source.bot)
      ? { bot_id: text(source.bot_id) ?? text(source.bot) }
      : {}),
    ...(text(source.status) ? { status: text(source.status) } : {}),
    ...(text(source.error) ? { error: text(source.error) } : {}),
    ...(nodeID ? { node_id: nodeID } : {}),
    ...(text(source.interaction_id) ?? text(checkpoint?.interaction_id)
      ? {
          interaction_id:
            text(source.interaction_id) ?? text(checkpoint?.interaction_id),
        }
      : {}),
    ...(questions ? { questions } : {}),
    ...(text(source.created_at) ? { created_at: text(source.created_at) } : {}),
    ...(text(source.updated_at) ? { updated_at: text(source.updated_at) } : {}),
    ...(attempts !== undefined ? { attempts } : {}),
    ...(numberValue(source.children_count) !== undefined
      ? { children_count: numberValue(source.children_count) }
      : {}),
  };
}

// Keep all wire-shape tolerance in this module. Views always receive one
// canonical root regardless of whether an early server build used `identity`
// instead of `board`, or wrapped the detail under a `pipeline_board` key.
export function normalizePipelineBoardDetail(value: unknown): PipelineBoardDetail {
  const outer = record(value) ?? {};
  const root = record(outer.pipeline_board) ?? outer;
  const boardSource = record(root.board) ?? record(root.identity) ?? root;
  const columns = Array.isArray(root.columns) ? root.columns : [];
  const cards = Array.isArray(root.cards)
    ? root.cards
    : Array.isArray(root.tasks)
      ? root.tasks
      : [];
  return {
    board: normalizeIdentity(boardSource),
    columns: columns.map(normalizeColumn),
    cards: cards.map(normalizePipelineBoardCard),
    ...(text(root.generated_at) ? { generated_at: text(root.generated_at) } : {}),
    ...(text(root.topology_error)
      ? { topology_error: text(root.topology_error) }
      : {}),
  };
}

export function normalizePipelineBoardList(value: unknown): PipelineBoardListItem[] {
  const root = record(value);
  const items = Array.isArray(value)
    ? value
    : Array.isArray(root?.boards)
      ? root.boards
      : Array.isArray(root?.pipeline_boards)
        ? root.pipeline_boards
        : [];
  return items.map((item) => {
    const source = record(item) ?? {};
    const detail = normalizePipelineBoardDetail(source);
    const boardSource = record(source.board) ?? record(source.identity) ?? source;
    const hasColumns = Array.isArray(source.columns);
    const hasCards = Array.isArray(source.cards) || Array.isArray(source.tasks);
    const columnCount =
      numberValue(source.column_count) ?? (hasColumns ? detail.columns.length : undefined);
    const cardCount =
      numberValue(source.card_count) ?? (hasCards ? detail.cards.length : undefined);
    const explicitAwaiting = numberValue(source.awaiting_input_count);
    const awaiting =
      explicitAwaiting ??
      (hasCards
        ? detail.cards.filter((card) => card.status === "paused_waiting_human").length
        : undefined);
    return {
      board: normalizeIdentity(boardSource),
      ...(columnCount !== undefined ? { column_count: columnCount } : {}),
      ...(cardCount !== undefined ? { card_count: cardCount } : {}),
      ...(awaiting !== undefined ? { awaiting_input_count: awaiting } : {}),
      ...(text(source.generated_at)
        ? { generated_at: text(source.generated_at) }
        : {}),
      ...(text(source.topology_error)
        ? { topology_error: text(source.topology_error) }
        : {}),
    };
  });
}

export async function listPipelineBoards(opts?: {
  signal?: AbortSignal;
}): Promise<PipelineBoardListItem[]> {
  const raw = await apiRequest<unknown>(BASE, { signal: opts?.signal });
  return normalizePipelineBoardList(raw);
}

export async function getPipelineBoard(
  botID: string,
  opts?: { signal?: AbortSignal },
): Promise<PipelineBoardDetail> {
  const raw = await apiRequest<unknown>(`${BASE}/${encodeURIComponent(botID)}`, {
    signal: opts?.signal,
  });
  return normalizePipelineBoardDetail(raw);
}

export async function createPipelineTask(
  botID: string,
  input: CreatePipelineTaskInput,
): Promise<NativeIssue> {
  return apiRequest<NativeIssue>(
    `${BASE}/${encodeURIComponent(botID)}/tasks`,
    {
      method: "POST",
      body: JSON.stringify(input),
    },
  );
}
