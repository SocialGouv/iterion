// Host-owned catalogue and policy engine for assistant actions.
//
// A model may REQUEST one of these ids, but it cannot add an action or relax
// its policy. The Studio owns the target validation and the execution. New
// actions deliberately default to "ask": installing a newer Studio must never
// silently expand an assistant's autonomy.

import { useEffect, useState } from "react";

import { readJSONFlag, writeJSONFlag } from "@/lib/localStorageFlag";

export const ASSISTANT_ACTION_POLICIES_KEY =
  "iterion.assistant.actionPolicies.v1";

export const ASSISTANT_ACTION_POLICY_VALUES = [
  "deny",
  "ask",
  "explicit",
  "allow",
] as const;

export type AssistantActionPolicy =
  (typeof ASSISTANT_ACTION_POLICY_VALUES)[number];

export type AssistantActionId = "editor.apply" | "editor.save";

export interface AssistantActionDefinition {
  id: AssistantActionId;
  label: string;
  description: string;
  risk: "reversible" | "persistent";
  defaultPolicy: AssistantActionPolicy;
}

export const ASSISTANT_ACTIONS: readonly AssistantActionDefinition[] = [
  {
    id: "editor.apply",
    label: "Apply changes to the open bot",
    description:
      "Replace the matching live editor buffer after revision and validation checks. The file is not saved.",
    risk: "reversible",
    defaultPolicy: "ask",
  },
  {
    id: "editor.save",
    label: "Save the open bot",
    description:
      "Write the validated live buffer to the file already bound to its editor tab. The assistant never chooses a path.",
    risk: "persistent",
    defaultPolicy: "ask",
  },
] as const;

export const ASSISTANT_ACTION_POLICY_OPTIONS: ReadonlyArray<{
  value: AssistantActionPolicy;
  label: string;
}> = [
  { value: "deny", label: "Never allow" },
  { value: "ask", label: "Always ask" },
  { value: "explicit", label: "Allow when explicitly requested" },
  { value: "allow", label: "Always allow" },
];

export type AssistantActionDecision = "deny" | "confirm" | "auto";

const POLICY_EVENT = "iterion:assistant-action-policy";

function isPolicy(value: unknown): value is AssistantActionPolicy {
  return (
    typeof value === "string" &&
    (ASSISTANT_ACTION_POLICY_VALUES as readonly string[]).includes(value)
  );
}

function definitionFor(id: AssistantActionId): AssistantActionDefinition {
  // AssistantActionId is derived from this closed host catalogue. Keep a
  // defensive fallback because persisted data is still untrusted input.
  const definition = ASSISTANT_ACTIONS.find((action) => action.id === id);
  if (definition) return definition;
  throw new Error(`Unknown host assistant action: ${id}`);
}

function storedPolicies(): Partial<Record<AssistantActionId, unknown>> {
  const value = readJSONFlag<unknown>(ASSISTANT_ACTION_POLICIES_KEY, {});
  if (!value || typeof value !== "object" || Array.isArray(value)) return {};
  return value as Partial<Record<AssistantActionId, unknown>>;
}

export function readAssistantActionPolicy(
  id: AssistantActionId,
): AssistantActionPolicy {
  const stored = storedPolicies()[id];
  return isPolicy(stored) ? stored : definitionFor(id).defaultPolicy;
}

export function writeAssistantActionPolicy(
  id: AssistantActionId,
  policy: AssistantActionPolicy,
): void {
  if (!isPolicy(policy)) return;
  const current = storedPolicies();
  writeJSONFlag(ASSISTANT_ACTION_POLICIES_KEY, {
    ...current,
    [id]: policy,
  });
  if (typeof window !== "undefined") {
    window.dispatchEvent(new CustomEvent(POLICY_EVENT, { detail: { id } }));
  }
}

/** Resolve policy only after a host-known action has actually been requested. */
export function decideAssistantAction(
  policy: AssistantActionPolicy,
  explicitlyRequested: boolean,
): AssistantActionDecision {
  switch (policy) {
    case "deny":
      return "deny";
    case "ask":
      return "confirm";
    case "explicit":
      return explicitlyRequested ? "auto" : "confirm";
    case "allow":
      return "auto";
  }
}

export function useAssistantActionPolicy(
  id: AssistantActionId,
): AssistantActionPolicy {
  const [policy, setPolicy] = useState(() => readAssistantActionPolicy(id));

  useEffect(() => {
    const refresh = () => setPolicy(readAssistantActionPolicy(id));
    window.addEventListener(POLICY_EVENT, refresh);
    // Native storage events cover another Studio tab; POLICY_EVENT covers
    // writes in this tab, because browsers do not send storage back to it.
    window.addEventListener("storage", refresh);
    return () => {
      window.removeEventListener(POLICY_EVENT, refresh);
      window.removeEventListener("storage", refresh);
    };
  }, [id]);

  return policy;
}
