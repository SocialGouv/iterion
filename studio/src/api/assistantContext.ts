import { apiRequest } from "./client";

export interface ResolvedAssistantTask {
  id: string;
  title?: string;
  state?: string;
  last_run_id?: string;
  awaiting_input?: boolean;
}

export interface ResolvedAssistantRun {
  id: string;
  status: string;
  workflow_name?: string;
  resumable?: boolean;
  rewindable?: boolean;
  failing_node?: string;
  error_code?: string;
  error?: string;
  repair?: {
    /** Source-package delegation capability; unrelated to engine rewind. */
    repairable: boolean;
    reason?: string;
    source_project_id?: string;
    source_ref?: string;
    package?: string;
    workflow_path?: string;
  };
}

export interface ResolvedAssistantReference {
  reference: string;
  resolved: boolean;
  kind?: string;
  reason?: string;
  task?: ResolvedAssistantTask;
  run?: ResolvedAssistantRun;
}

export interface ResolvedAssistantContext {
  references: ResolvedAssistantReference[];
}

export function resolveAssistantContext(
  references: readonly string[],
  message = "",
): Promise<ResolvedAssistantContext> {
  return apiRequest<ResolvedAssistantContext>(
    "/api/v1/assistant/context/resolve",
    {
      method: "POST",
      body: JSON.stringify({ references, message }),
    },
  );
}
