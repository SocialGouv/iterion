// The operator's remembered model choice for a long-lived surface.
//
// Mirrors pkg/modelprefs over /api/v1/preferences/model. The `key` is an
// opaque scope string the client chooses (the assistant passes its bot id);
// the server never interprets it, which is what lets a second conversational
// bot exist without an engine change.

import { request } from "./client";

export interface ModelPref {
  key: string;
  model?: string;
  backend?: string;
  // reasoning_effort (low|medium|high|xhigh|max|ultracode). Rejected with a
  // 400 when it is not one of those.
  effort?: string;
  // set distinguishes "never recorded" (fall back to the bot's own defaults)
  // from "recorded, and deliberately empty".
  set: boolean;
}

export async function fetchModelPref(
  key: string,
  opts: { signal?: AbortSignal } = {},
): Promise<ModelPref> {
  return request<ModelPref>(
    `/v1/preferences/model?key=${encodeURIComponent(key)}`,
    { method: "GET", signal: opts.signal },
  );
}

export async function saveModelPref(
  pref: Omit<ModelPref, "set">,
): Promise<ModelPref> {
  return request<ModelPref>("/v1/preferences/model", {
    method: "PUT",
    // Omitted dimensions CLEAR the stored ones server-side, which is how the
    // operator returns to the bot's default — so send them explicitly rather
    // than stripping the empties.
    body: JSON.stringify({
      key: pref.key,
      model: pref.model ?? "",
      backend: pref.backend ?? "",
      effort: pref.effort ?? "",
    }),
  });
}

export async function clearModelPref(key: string): Promise<ModelPref> {
  return request<ModelPref>(
    `/v1/preferences/model?key=${encodeURIComponent(key)}`,
    { method: "DELETE" },
  );
}

// modelPrefOverrides turns a preference into the `model_overrides` a launch
// takes. The selector is "agent": the operator is choosing the answering
// model, not retargeting internal judges. Keeping judges on their declared
// model preserves cross-family review. Returns undefined when nothing is
// chosen, so the bot's own DSL defaults apply untouched.
export function modelPrefOverrides(
  pref: Pick<ModelPref, "model" | "backend" | "effort"> | null | undefined,
): { selector: string; model?: string; backend?: string; effort?: string }[] | undefined {
  if (!pref) return undefined;
  const model = pref.model?.trim() || undefined;
  const backend = pref.backend?.trim() || undefined;
  const effort = pref.effort?.trim() || undefined;
  if (!model && !backend && !effort) return undefined;
  return [{ selector: "agent", model, backend, effort }];
}
