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
export type TriggerSource = "forge" | "schedule" | "board" | "run" | "manual" | "custom";

// InvocationKind mirrors bundle.InvocationKind.
export type TriggerInvocation = "forge" | "command" | "schedule" | "board";

export type TriggerMode = "direct" | "board" | "";

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
  vars?: Record<string, string>;
  args_var?: string;
  cron?: string;
  origin?: string;
  enabled: boolean;
  created_at?: string;
  updated_at?: string;
}

// SubscriptionInput is the create/update payload (server-managed fields
// omitted).
export interface SubscriptionInput {
  repo?: string;
  bot_id: string;
  invocation?: TriggerInvocation;
  mode?: TriggerMode;
  match: TriggerMatcher;
  vars?: Record<string, string>;
  args_var?: string;
  cron?: string;
  enabled?: boolean;
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

export function createTrigger(input: SubscriptionInput): Promise<TriggerSubscription> {
  return request<TriggerSubscription>("", { method: "POST", body: JSON.stringify(input) });
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

// setTriggerEnabled is a convenience PUT that flips only the enabled flag,
// preserving the rest of the subscription.
export function setTriggerEnabled(sub: TriggerSubscription, enabled: boolean): Promise<TriggerSubscription> {
  return updateTrigger(sub.id, {
    repo: sub.repo,
    bot_id: sub.bot_id,
    invocation: sub.invocation,
    mode: sub.mode,
    match: sub.match,
    vars: sub.vars,
    args_var: sub.args_var,
    cron: sub.cron,
    enabled,
  });
}
