// Event-driven trigger subscription REST client. Mirrors
// pkg/server/triggers_routes.go (/api/v1/triggers). Local single-host scope
// (tenant ""); the cloud team-scoped variant is a follow-on.

import { apiRequest, guard404, FeatureUnavailableError } from "./client";

export { FeatureUnavailableError };

const BASE = "/api/v1/triggers";

function request<T>(path: string, init?: RequestInit): Promise<T> {
  return apiRequest<T>(BASE + path, init);
}

// Source classifies where a trigger event originates (mirrors trigger.Source).
export type TriggerSource =
  | "forge"
  | "schedule"
  | "board"
  | "run"
  | "manual"
  | "custom"
  | "connector";

// InvocationKind mirrors bundle.InvocationKind.
export type TriggerInvocation =
  | "forge"
  | "command"
  | "schedule"
  | "board"
  | "keepalive"
  | "connector";

// ExecutionMode mirrors bundle.ExecutionMode. "deliver" is accepted in a
// manifest but refused by every effect until #3136 (bundle.ResolveMode).
export type TriggerMode = "direct" | "board" | "deliver" | "";

// Matcher mirrors trigger.Matcher: empty slice = match-any per dimension,
// labels = match-all.
export interface TriggerMatcher {
  sources?: TriggerSource[];
  kinds?: string[];
  actions?: string[];
  repos?: string[];
  authors?: string[];
  labels?: string[];
  subject_states?: string[];
}

// Subscription mirrors trigger.Subscription.
export interface TriggerSubscription {
  id: string;
  tenant_id?: string;
  repo?: string;
  bot_id: string;
  invocation: TriggerInvocation;
  mode?: TriggerMode;
  match: TriggerMatcher;
  // Connector binding (invocation "connector", ADR-168 §6): which
  // connection's which declared trigger the subscription listens to.
  // connector_key is derived server-side from the two, never sent.
  connection_id?: string;
  trigger_id?: string;
  connector_key?: string;
  // A polled trigger's binding (ADR-168 §7): poll_query is what the
  // operator typed for its query fields, backfill how many of the poller's
  // past items the subscription receives once at activation. poller,
  // poller_since and backfill_done are server-managed: the poller is derived
  // from (connection, trigger, poll_query), never sent.
  poller?: string;
  poll_query?: Record<string, string>;
  poller_since?: number;
  backfill?: number;
  backfill_done?: boolean;
  filters?: Record<string, string[]>;
  when?: string;
  var_map?: Record<string, string>;
  no_live_run?: "" | "launch" | "drop";
  vars?: Record<string, string>;
  args_var?: string;
  cron?: string;
  // Overlap policy + guard for schedule-kind subscriptions (pkg/schedgate).
  overlap?: string;
  max_concurrent?: number;
  guard?: string;
  guard_timeout?: string;
  guard_var?: string;
  origin?: string;
  enabled: boolean;
  // Launch-time verdict of the last direct launch this subscription asked
  // for: empty means it went through, a message says why it did not (an org
  // launch-gate denial, a run service that refused the run).
  last_error?: string;
  last_error_at?: string;
  // Terminal outcome of the last RUN this subscription launched (#1501).
  // last_error above is the launch decision; these are the run's result —
  // without them a subscription that fired and whose run died reads exactly
  // like one that fired and worked. last_run_error_code is a stable
  // machine-readable classifier (a store.FailureCode, e.g.
  // "SANDBOX_DRIVER_UNAVAILABLE"); both error fields are cleared by a
  // successful run.
  last_run_id?: string;
  last_run_status?: string;
  last_run_error?: string;
  last_run_error_code?: string;
  last_run_at?: string;
}

// SubscriptionInput is the create/update payload (server-managed fields
// omitted).
export interface SubscriptionInput {
  repo?: string;
  bot_id: string;
  invocation?: TriggerInvocation;
  mode?: TriggerMode;
  match: TriggerMatcher;
  // Connector binding (invocation "connector"): validated at save against
  // the connection, its package's trigger and the bot; the server owns the
  // match and the connector_key.
  connection_id?: string;
  trigger_id?: string;
  // A polled trigger's query values and backfill (≥ 0). No poller: the
  // server derives it and refuses a request that names one.
  poll_query?: Record<string, string>;
  backfill?: number;
  filters?: Record<string, string[]>;
  when?: string;
  var_map?: Record<string, string>;
  no_live_run?: "" | "launch" | "drop";
  vars?: Record<string, string>;
  args_var?: string;
  cron?: string;
  overlap?: string;
  max_concurrent?: number;
  guard?: string;
  guard_timeout?: string;
  guard_var?: string;
  enabled?: boolean;
}

// toSubscriptionInput projects a subscription onto the full update payload.
// PUT replaces every request field (only id/origin/timestamps are preserved
// server-side), so partial edits must start from this projection or they
// silently clear vars / schedgate policy. Server-managed fields stay out:
// a request naming a poller is refused, the query names it.
export function toSubscriptionInput(sub: TriggerSubscription): SubscriptionInput {
  return {
    repo: sub.repo,
    bot_id: sub.bot_id,
    invocation: sub.invocation,
    mode: sub.mode,
    match: sub.match,
    connection_id: sub.connection_id,
    trigger_id: sub.trigger_id,
    poll_query: sub.poll_query,
    backfill: sub.backfill,
    filters: sub.filters,
    when: sub.when,
    var_map: sub.var_map,
    no_live_run: sub.no_live_run,
    vars: sub.vars,
    args_var: sub.args_var,
    cron: sub.cron,
    overlap: sub.overlap,
    max_concurrent: sub.max_concurrent,
    guard: sub.guard,
    guard_timeout: sub.guard_timeout,
    guard_var: sub.guard_var,
    enabled: sub.enabled,
  };
}

interface ListResponse {
  subscriptions: TriggerSubscription[];
}

// listTriggers returns the local-scope subscriptions, optionally filtered by
// repo or bot (the "by repo / by bot" views).
export function listTriggers(filter?: { repo?: string; bot?: string }): Promise<TriggerSubscription[]> {
  const q = new URLSearchParams();
  if (filter?.repo) q.set("repo", filter.repo);
  if (filter?.bot) q.set("bot", filter.bot);
  const suffix = q.toString() ? `?${q.toString()}` : "";
  return guard404("triggers", () => request<ListResponse>(suffix)).then((r) => r.subscriptions ?? []);
}

// TriggerCreated is the create answer: the stored row plus, only when the
// server could not re-check an api key / stored secret its pins name once
// the row was stored, the ids of those records — the row exists under a
// fresh id, so the answer stays a 201 and this receipt asks the operator to
// re-read those records and clear a pin naming a deleted one.
export type TriggerCreated = TriggerSubscription & { pins_unreconciled?: string[] };

export function createTrigger(input: SubscriptionInput): Promise<TriggerCreated> {
  return request<TriggerCreated>("", { method: "POST", body: JSON.stringify(input) });
}

export function updateTrigger(id: string, input: SubscriptionInput): Promise<TriggerSubscription> {
  return request<TriggerSubscription>(`/${encodeURIComponent(id)}`, {
    method: "PUT",
    body: JSON.stringify(input),
  });
}

export function deleteTrigger(id: string): Promise<void> {
  return request<void>(`/${encodeURIComponent(id)}`, { method: "DELETE" });
}

// createTriggerFromInvocation is the bot home's one-click "enable this
// trigger": POST /api/v1/bots/{name}/triggers/from-invocation derives a
// subscription server-side from the bot's manifest invocation at `index`
// (schedule, board or keepalive kinds). `cron` optionally overrides a schedule
// invocation's suggested_cron. Errors surface as ApiError: 409 = a
// bot-home subscription for that kind already exists (body carries
// `subscription_id`), 400 = explicit reason (command/forge kinds must be
// wired through the forge integration; board without a board: block;
// schedule without a cron).
export function createTriggerFromInvocation(
  botName: string,
  index: number,
  cron?: string,
): Promise<TriggerSubscription> {
  return apiRequest<TriggerSubscription>(
    `/api/v1/bots/${encodeURIComponent(botName)}/triggers/from-invocation`,
    {
      method: "POST",
      body: JSON.stringify({ index, ...(cron ? { cron } : {}) }),
    },
  );
}

// setTriggerEnabled is a convenience PUT that flips only the enabled flag,
// preserving the rest of the subscription.
export function setTriggerEnabled(sub: TriggerSubscription, enabled: boolean): Promise<TriggerSubscription> {
  return updateTrigger(sub.id, { ...toSubscriptionInput(sub), enabled });
}

// TriggerPreview mirrors the server's triggerPreview: what the lane would
// decide and the bot would receive, for a pasted delivery or the trigger's
// own sample. Writes and launches nothing.
export interface TriggerPreviewResult {
  sample?: boolean;
  filtered?: string;
  matched: boolean;
  reason?: string;
  refusal?: string;
  vars?: Record<string, string>;
  event?: unknown;
}

// triggerPreviewReq mirrors triggerPreviewReq (pkg/server/triggers_connector.go).
export interface TriggerPreviewInput {
  bot_id?: string;
  connection_id?: string;
  trigger_id?: string;
  filters?: Record<string, string[]>;
  when?: string;
  var_map?: Record<string, string>;
  vars?: Record<string, string>;
  // A polled trigger's query values, as the subscription would carry them.
  poll_query?: Record<string, string>;
  // An OBJECT (the server decodes into a map) — a JSON string is a 400. For
  // a polled trigger it is ONE ITEM of the operation's answer, not a
  // delivery, and headers are ignored.
  payload?: unknown;
  headers?: Record<string, string>;
}

export async function previewTrigger(input: TriggerPreviewInput): Promise<TriggerPreviewResult> {
  return apiRequest<TriggerPreviewResult>(`${BASE}/preview`, {
    method: "POST",
    body: JSON.stringify(input),
  });
}
