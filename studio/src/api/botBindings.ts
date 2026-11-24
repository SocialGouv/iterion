// Bot-secret binding REST client. Mirrors pkg/server/bot_bindings_routes.go.

import { FeatureUnavailableError, guard404, request } from "./client";

export { FeatureUnavailableError };

// The two kinds a binding carries (ADR-147): absent kind reads "secret"
// (every binding stored before kinds existed), so the UI never branches on
// the raw field — effectiveKind below is the only read.
export type BindingKind = "secret" | "context";

export function effectiveKind(b: Pick<BotSecretBinding, "kind">): BindingKind {
  return b.kind === "context" ? "context" : "secret";
}

// ContextSource is the `context` kind's payload: a file inside a git
// repository the operator pins (tag or sha), materialised as a run
// attachment of every run of the bot under the binding's declared name.
export interface ContextSource {
  git_url: string;
  ref: string;
  path: string;
  // The READ credential (a team secret id), never the material; empty for
  // a public repository.
  secret_id?: string;
}

export interface BotSecretBinding {
  id: string;
  tenant_id: string;
  bot_id: string;
  kind?: string;
  secret_id: string;
  secret_name_for_workflow: string;
  context?: ContextSource;
  allowed_hosts?: string[];
  // The engine's readout: set when the last launch could not materialise
  // this binding (a context whose pin did not resolve, a name the workflow
  // never declared). Cleared by the next launch that succeeds.
  degraded_reason?: string;
  degraded_at?: string;
  created_by: string;
  created_at: string;
  updated_at: string;
}

export interface CreateBindingInput {
  kind?: BindingKind;
  secret_id?: string;
  secret_name_for_workflow: string;
  allowed_hosts?: string[];
  context?: ContextSource;
}

export interface UpdateBindingInput {
  secret_id?: string;
  secret_name_for_workflow?: string;
  allowed_hosts?: string[];
  context?: ContextSource;
}

export async function listBindings(
  teamID: string,
  botID: string,
): Promise<BotSecretBinding[]> {
  return guard404("bindings", async () => {
    const r = await request<{ bindings: BotSecretBinding[] }>(
      `/teams/${encodeURIComponent(teamID)}/bots/${encodeURIComponent(botID)}/bindings`,
    );
    return r.bindings ?? [];
  });
}

export function createBinding(
  teamID: string,
  botID: string,
  input: CreateBindingInput,
): Promise<BotSecretBinding> {
  return guard404("bindings", () =>
    request<BotSecretBinding>(
      `/teams/${encodeURIComponent(teamID)}/bots/${encodeURIComponent(botID)}/bindings`,
      { method: "POST", body: JSON.stringify(input) },
    ),
  );
}

export function updateBinding(
  teamID: string,
  botID: string,
  bindingID: string,
  input: UpdateBindingInput,
): Promise<BotSecretBinding> {
  return guard404("bindings", () =>
    request<BotSecretBinding>(
      `/teams/${encodeURIComponent(teamID)}/bots/${encodeURIComponent(botID)}/bindings/${encodeURIComponent(bindingID)}`,
      { method: "PATCH", body: JSON.stringify(input) },
    ),
  );
}

export async function deleteBinding(
  teamID: string,
  botID: string,
  bindingID: string,
): Promise<void> {
  await guard404("bindings", () =>
    request<void>(
      `/teams/${encodeURIComponent(teamID)}/bots/${encodeURIComponent(botID)}/bindings/${encodeURIComponent(bindingID)}`,
      { method: "DELETE" },
    ),
  );
}
