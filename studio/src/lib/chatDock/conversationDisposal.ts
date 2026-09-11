// One cancellation contract for every UI action that abandons an assistant
// conversation: close tab, start a new session, or switch bot.
//
// The server owns run liveness. Client snapshots are deliberately NOT used to
// decide whether cancellation should be sent: they can be absent during
// hydration and stale after a websocket gap. POST /cancel is idempotent, so a
// known run id is enough.

import { cancelRun, type RunStatus } from "@/api/runs";
import { ApiError } from "@/api/client";

import type { Conversation } from "./conversations";

export interface ConversationSnapshotRef {
  run?: {
    id?: string;
    status?: RunStatus;
  };
}

/** Prefer the durable conversation owner; use the in-memory snapshot only for legacy/racy tabs. */
export function runIdForDisposal(
  conversation: Conversation | null | undefined,
  snapshot: ConversationSnapshotRef | null | undefined,
): string | null {
  return conversation?.runId || snapshot?.run?.id || null;
}

// A missing run means the desired end state already holds. 410 is accepted
// too for cloud/proxy deployments that distinguish pruned from never-found.
export function isRunAlreadyGoneError(error: unknown): boolean {
  if (error instanceof ApiError) return error.status === 404 || error.status === 410;
  const message = error instanceof Error ? error.message : String(error ?? "");
  return /API error (404|410)(?:\D|$)/i.test(message);
}

/**
 * Whether abandoning this run deserves destructive confirmation.
 *
 * An unknown status is treated as live. failed_resumable also needs consent:
 * cancelling it discards the operator's ability to resume its checkpoint.
 */
export function shouldConfirmRunDisposal(
  runId: string | null,
  status: RunStatus | null | undefined,
): boolean {
  if (!runId) return false;
  return status !== "finished" && status !== "failed" && status !== "cancelled";
}

export type CancelRunRequest = typeof cancelRun;

/**
 * Ask the server to stop a known run, then dispose the client owner.
 *
 * Resolution means the server accepted the request (often 202/cancelling),
 * not that a websocket has already observed `cancelled`. Waiting for that
 * event would strand a closing tab whenever its socket is the broken part.
 */
export async function cancelThenDispose({
  runId,
  dispose,
  cancel = cancelRun,
}: {
  runId: string | null;
  dispose: () => void;
  cancel?: CancelRunRequest;
}): Promise<void> {
  if (runId) {
    try {
      await cancel(runId);
    } catch (error) {
      if (!isRunAlreadyGoneError(error)) throw error;
    }
  }
  dispose();
}
