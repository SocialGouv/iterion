import { knownRunBirthWire, markRunLost, RUN_ORIGIN_LOST } from "@/lib/lostRuns";

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

// withReferenceBirth spells a run or node reference with the birth the page
// knows for its run — `run/<id>@<birth>`, `node/<id>@<birth>/<node>` — so
// the server resolves the run the page meant, or nothing: a run replaced
// under its ID answers reason run_origin_lost. Other references, and runs
// whose birth the page never learned, are sent unchanged.
export function withReferenceBirth(ref: string): string {
  const [kind, rest] = [ref.slice(0, ref.indexOf("/")), ref.slice(ref.indexOf("/") + 1)];
  if ((kind !== "run" && kind !== "node") || ref.indexOf("/") < 0 || rest.includes("@")) return ref;
  const cut = kind === "node" ? rest.indexOf("/") : -1;
  const runId = cut >= 0 ? rest.slice(0, cut) : rest;
  const birth = knownRunBirthWire("", runId);
  if (birth === undefined || !runId) return ref;
  return `${kind}/${runId}@${birth}${cut >= 0 ? rest.slice(cut) : ""}`;
}

export async function resolveAssistantContext(
  references: readonly string[],
  message = "",
): Promise<ResolvedAssistantContext> {
  // The sent spelling maps back to the caller's: the server echoes what it
  // received.
  const sent = new Map<string, string>();
  const wire = references.map((ref) => {
    const spelled = withReferenceBirth(ref);
    sent.set(spelled, ref);
    return spelled;
  });
  const out = await apiRequest<ResolvedAssistantContext>(
    "/api/v1/assistant/context/resolve",
    {
      method: "POST",
      body: JSON.stringify({ references: wire, message }),
    },
  );
  for (const resolved of out.references ?? []) {
    const original = sent.get(resolved.reference);
    if (original === undefined) continue;
    if (resolved.reason === RUN_ORIGIN_LOST) {
      const runId = original.split("/")[1];
      if (runId) markRunLost("", runId);
    }
    resolved.reference = original;
  }
  return out;
}
