// Conductor REST client — mirrors pkg/conductor/manager.go +
// pkg/conductor/state.go. Covers both lifecycle (status / config /
// start / pause / resume / stop) and operational (state / refresh /
// reload / issue cancel / ws) endpoints.

import { apiRequest } from "./client";

const BASE = "/api/v1/conductor";

function request<T>(path: string, init?: RequestInit): Promise<T> {
  return apiRequest<T>(BASE + path, init);
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

export type ManagerState = "idle" | "running" | "paused" | "error";

export interface ManagerStatus {
  state: ManagerState;
  has_config: boolean;
  started_at?: string;
  last_error?: string;
}

export function getStatus(): Promise<ManagerStatus> {
  return request("/status");
}

export function start(): Promise<ManagerStatus> {
  return request("/start", { method: "POST" });
}

export function stop(): Promise<ManagerStatus> {
  return request("/stop", { method: "POST" });
}

export function pause(): Promise<ManagerStatus> {
  return request("/pause", { method: "POST" });
}

export function resume(): Promise<ManagerStatus> {
  return request("/resume", { method: "POST" });
}

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

// Mirror pkg/conductor/config.go. snake_case throughout — same wire
// shape on both YAML and JSON sides.
export type TrackerKind = "native" | "github" | "forgejo";

export interface LabelSelector {
  labels_include?: string[];
  labels_exclude?: string[];
}

export interface GitHubTrackerConfig {
  repo: string;
  token?: string;
  state_mapping?: Record<string, LabelSelector>;
  claimed_label?: string;
  include_labels?: string[];
  exclude_labels?: string[];
}

export interface ForgejoTrackerConfig {
  host: string;
  repo: string;
  token?: string;
  state_mapping?: Record<string, LabelSelector>;
  claimed_label?: string;
  include_labels?: string[];
  exclude_labels?: string[];
}

export interface TrackerConfig {
  kind: TrackerKind;
  native?: Record<string, never>;
  github?: GitHubTrackerConfig;
  forgejo?: ForgejoTrackerConfig;
}

export interface DispatchConfig {
  vars?: Record<string, string>;
  attachments?: Record<string, string>;
}

export interface PollingConfig {
  interval_ms?: number;
}

export interface AgentConfig {
  max_concurrent?: number;
  max_concurrent_by_state?: Record<string, number>;
  max_turns?: number;
  max_retry_backoff_ms?: number;
}

export type WorkspacePersistPolicy = "" | "keep" | "cleanup_on_done" | "cleanup_on_terminal";

export interface WorkspaceConfig {
  root?: string;
  persist?: WorkspacePersistPolicy;
}

export interface HookSpec {
  script?: string;
  path?: string;
  timeout_ms?: number;
}

export interface HooksConfig {
  after_create?: HookSpec | null;
  before_run?: HookSpec | null;
  after_run?: HookSpec | null;
  before_remove?: HookSpec | null;
}

export interface StallConfig {
  timeout_ms?: number;
}

export interface ServerConfig {
  port?: number;
}

export interface ConductorConfig {
  name?: string;
  workflow: string;
  tracker: TrackerConfig;
  dispatch?: DispatchConfig;
  polling?: PollingConfig;
  agent?: AgentConfig;
  workspace?: WorkspaceConfig;
  hooks?: HooksConfig;
  stall?: StallConfig;
  server?: ServerConfig;
}

export async function getConfig(): Promise<ConductorConfig | null> {
  try {
    return await request<ConductorConfig>("/config");
  } catch (err: unknown) {
    // 404 = no config persisted yet → null is a meaningful "empty"
    // state the form binds to.
    const msg = err instanceof Error ? err.message : String(err);
    if (msg.includes(" 404")) return null;
    throw err;
  }
}

export function saveConfig(cfg: ConductorConfig): Promise<ConductorConfig> {
  return request("/config", { method: "PUT", body: JSON.stringify(cfg) });
}

// ---------------------------------------------------------------------------
// Operational
// ---------------------------------------------------------------------------

export interface ConductorSnapshot {
  name: string;
  tracker: string;
  generated_at: string;
  polling_interval_seconds: number;
  stall_timeout_seconds: number;
  paused: boolean;
  running: RunningView[] | null;
  retries: RetryView[] | null;
  slots: SlotsView;
}

export interface RunningView {
  issue_id: string;
  identifier: string;
  run_id: string;
  workflow_state: string;
  workspace_path?: string;
  started_at: string;
  last_event_at: string;
  last_event_name?: string;
  attempt?: number;
}

export interface RetryView {
  issue_id: string;
  identifier: string;
  attempt: number;
  due_at: string;
  error?: string;
}

export interface SlotsView {
  global_max: number;
  global_used: number;
  per_state_max?: Record<string, number>;
  per_state_used?: Record<string, number>;
}

export function getState(): Promise<ConductorSnapshot> {
  return request("/state");
}

export function refresh(): Promise<{ queued: boolean }> {
  return request("/refresh", { method: "POST" });
}

export function reload(): Promise<{ reloaded: boolean; polling_interval_s: number }> {
  return request("/reload", { method: "POST" });
}

export function cancelIssue(id: string): Promise<{ status: string }> {
  return request(`/issues/${encodeURIComponent(id)}/cancel`, { method: "POST" });
}

// openWS returns a connected WebSocket that broadcasts Snapshot
// updates. The caller must close it. Reconnection is the caller's
// responsibility.
export function openWS(): WebSocket {
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  const url = `${proto}//${window.location.host}${BASE}/ws`;
  return new WebSocket(url);
}
