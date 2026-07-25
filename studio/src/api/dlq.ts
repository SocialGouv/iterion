// Dead-letter-queue admin REST client. Mirrors pkg/server/queue_sweeper.go
// (DLQ admin REST) + pkg/queue/nats/dlq.go (the DLQMessage admin view).
// Super-admin, cloud-mode only — the routes are registered only when a
// NATS queue connection is wired.

import { FeatureUnavailableError, guard404, request } from "./client";

export { FeatureUnavailableError };

// One parked message, as listed. Headers stamped at park time explain
// WHY the run landed here without decoding the payload.
export interface DLQMessage {
  seq: number;
  run_id: string;
  tenant_id: string;
  reason: string;
  num_delivered?: string;
  parked_at: string; // RFC3339
  size_bytes: number;
}

export interface DLQListResponse {
  // The Go handler marshals an empty list as `messages: null`.
  messages: DLQMessage[] | null;
  // Cursor to continue from; 0 = exhausted.
  next_cursor: number;
}

export interface DLQPeekResponse {
  message: DLQMessage;
  // Raw parked payload (the serialized RunMessage) for inspection.
  payload: unknown;
}

// listDLQ pages from cursor (0 = oldest). Only the list is guard404'd:
// an unrouted /api/admin/dlq (no queue wired) is the feature gate; a
// per-seq 404 on the other calls means "that message is gone", which
// callers surface as a plain error.
export function listDLQ(cursor = 0, limit = 50): Promise<DLQListResponse> {
  const sp = new URLSearchParams();
  if (cursor > 0) sp.set("cursor", String(cursor));
  if (limit > 0) sp.set("limit", String(limit));
  const s = sp.toString();
  return guard404("dlq", () =>
    request<DLQListResponse>(`/admin/dlq${s ? `?${s}` : ""}`),
  );
}

export function peekDLQ(seq: number): Promise<DLQPeekResponse> {
  return request<DLQPeekResponse>(`/admin/dlq/${seq}`);
}

export interface DLQReplayResponse {
  status: "replayed";
  run_id: string;
}

// replayDLQ re-enqueues the parked message onto the live runs subject
// and removes it from the DLQ.
export function replayDLQ(seq: number): Promise<DLQReplayResponse> {
  return request<DLQReplayResponse>(`/admin/dlq/${seq}/replay`, {
    method: "POST",
  });
}

// discardDLQ permanently deletes one parked message (204 No Content).
export function discardDLQ(seq: number): Promise<void> {
  return request<void>(`/admin/dlq/${seq}`, { method: "DELETE" });
}
