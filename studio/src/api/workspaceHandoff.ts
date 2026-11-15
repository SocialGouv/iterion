import { knownRunBirthWire, LostRunError, lostRunFromResponse, lostRunStore, markRunLost } from "@/lib/lostRuns";
import { apiBase } from "@/lib/scope";

interface WorkspaceHandoffCreated {
  ticket: string;
  handoff_id: string;
  destination_project_id: string;
  destination_project_name: string;
}

interface WorkspaceHandoffRedeemed {
  handoff_id: string;
  source_project_id: string;
  destination_project_id: string;
  summary: string;
  bot_id: string;
}

export interface WorkspaceHandoffReceipt {
  status: "completed" | "failed";
  summary: string;
  changed_files?: string[];
  commit_sha?: string;
  branch?: string;
  pr_url?: string;
  next_step?: string;
}

// The handoff runs ride in request BODIES, so their known births ride beside
// them (source_run_birth, destination_run_birth): the host compares each
// right after loading the run and answers 409 run_origin_lost, naming it,
// for one replaced under its ID — mapped here to the page's lost registry.
async function post<T>(path: string, body: unknown): Promise<T> {
  const url = `${apiBase()}${path}`;
  const response = await fetch(url, {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!response.ok) {
    const lostRun = await lostRunFromResponse(url, response);
    if (lostRun !== null) {
      markRunLost(lostRunStore(url, lostRun), lostRun);
      throw new LostRunError(lostRun);
    }
    throw new Error(await response.text());
  }
  return (await response.json()) as T;
}

// withRunBirth adds `<field>` = the known birth of runId, when the page
// knows it (a pane's own runs live in its primary store).
function withRunBirth<T extends object>(body: T, field: string, runId: string): T {
  const birth = knownRunBirthWire("", runId);
  return birth === undefined ? body : { ...body, [field]: birth };
}

export function createWorkspaceHandoff(
  destinationProject: string,
  summary: string,
  sourceRunId: string,
) {
  return post<WorkspaceHandoffCreated>(
    "/workspace/handoffs",
    withRunBirth({ destination_project: destinationProject, summary, source_run_id: sourceRunId }, "source_run_birth", sourceRunId),
  );
}

export function redeemWorkspaceHandoff(
  ticket: string,
  destinationClientId: string,
  destinationConversationId: string,
) {
  return post<WorkspaceHandoffRedeemed>("/workspace/handoffs/redeem", {
    ticket,
    destination_client_id: destinationClientId,
    destination_conversation_id: destinationConversationId,
  });
}

export function bindWorkspaceHandoff(
  handoffId: string,
  destinationRunId: string,
) {
  return post<{ handoff_id: string; bound: boolean }>(
    "/workspace/handoffs/bind",
    withRunBirth({ handoff_id: handoffId, destination_run_id: destinationRunId }, "destination_run_birth", destinationRunId),
  );
}

export function completeWorkspaceHandoff(
  handoffId: string,
  destinationRunId: string,
  receipt: WorkspaceHandoffReceipt,
) {
  return post<{
    handoff_id: string;
    receipt_id: string;
    delivery_status: "pending" | "accepted" | "delivered";
    idempotent: boolean;
  }>(
    "/workspace/handoffs/complete",
    withRunBirth({ handoff_id: handoffId, destination_run_id: destinationRunId, ...receipt }, "destination_run_birth", destinationRunId),
  );
}
