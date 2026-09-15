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

async function post<T>(path: string, body: unknown): Promise<T> {
  const response = await fetch(`${apiBase()}${path}`, {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!response.ok) throw new Error(await response.text());
  return (await response.json()) as T;
}

export function createWorkspaceHandoff(
  destinationProject: string,
  summary: string,
  sourceRunId: string,
) {
  return post<WorkspaceHandoffCreated>("/workspace/handoffs", {
    destination_project: destinationProject,
    summary,
    source_run_id: sourceRunId,
  });
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
    { handoff_id: handoffId, destination_run_id: destinationRunId },
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
  }>("/workspace/handoffs/complete", {
    handoff_id: handoffId,
    destination_run_id: destinationRunId,
    ...receipt,
  });
}
