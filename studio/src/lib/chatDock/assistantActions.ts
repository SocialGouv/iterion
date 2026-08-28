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

export type AssistantActionId =
  | "editor.apply"
  | "editor.save"
  | "editor.files.save"
  | "board.issue.create"
  | "board.issue.update"
  | "board.issue.transition"
  | "board.issue.comment"
  | "board.issue.delete"
  | "pipeline.task.create"
  | "pipeline.task.update"
  | "pipeline.task.ready"
  | "pipeline.task.launch"
  | "pipeline.task.reset"
  | "pipeline.task.close"
  | "pipeline.task.delete"
  | "run.launch"
  | "run.pause"
  | "run.resume"
  | "run.cancel"
  | "run.rename"
  | "run.delete"
  | "dispatcher.start"
  | "dispatcher.pause"
  | "dispatcher.resume"
  | "dispatcher.stop"
  | "bot.create"
  | "bot.install"
  | "plugin.enable"
  | "plugin.disable"
  | "plugin.install"
  | "plugin.uninstall";

export type AssistantActionGroup =
  | "Editor"
  | "Board"
  | "Pipelines"
  | "Runs"
  | "Dispatcher"
  | "Bots"
  | "Plugins";

export interface AssistantActionDefinition {
  id: AssistantActionId;
  group: AssistantActionGroup;
  label: string;
  description: string;
  risk: "reversible" | "persistent" | "compute" | "destructive";
  defaultPolicy: AssistantActionPolicy;
}

export const ASSISTANT_ACTIONS: readonly AssistantActionDefinition[] = [
  {
    id: "editor.apply",
    group: "Editor",
    label: "Apply changes to the open bot",
    description:
      "Replace the matching live editor buffer after revision and validation checks. The file is not saved.",
    risk: "reversible",
    defaultPolicy: "ask",
  },
  {
    id: "editor.save",
    group: "Editor",
    label: "Save the open bot",
    description:
      "Write the validated live buffer to the file already bound to its editor tab. The assistant never chooses a path.",
    risk: "persistent",
    defaultPolicy: "ask",
  },
  {
    id: "editor.files.save",
    group: "Editor",
    label: "Save declared bot companion files",
    description:
      "Preview and persist exact replacements only in files declared by the open bot manifest, with stale-content checks.",
    risk: "persistent",
    defaultPolicy: "ask",
  },
  {
    id: "board.issue.create",
    group: "Board",
    label: "Create a board issue",
    description: "Create one issue from host-validated title, body, labels, owner and bot fields.",
    risk: "persistent",
    defaultPolicy: "ask",
  },
  {
    id: "board.issue.update",
    group: "Board",
    label: "Edit a board issue",
    description: "Update only the declared fields of an existing issue.",
    risk: "persistent",
    defaultPolicy: "ask",
  },
  {
    id: "board.issue.transition",
    group: "Board",
    label: "Move a board issue",
    description: "Transition an existing issue to a named board state.",
    risk: "reversible",
    defaultPolicy: "ask",
  },
  {
    id: "board.issue.comment",
    group: "Board",
    label: "Comment on a board issue",
    description: "Append an operator-visible comment to an existing issue.",
    risk: "persistent",
    defaultPolicy: "ask",
  },
  {
    id: "board.issue.delete",
    group: "Board",
    label: "Delete a board issue",
    description: "Permanently remove an issue from the board.",
    risk: "destructive",
    defaultPolicy: "ask",
  },
  {
    id: "pipeline.task.create",
    group: "Pipelines",
    label: "Create a pipeline task",
    description: "Create a bot-backed pipeline ticket, optionally staged or started.",
    risk: "persistent",
    defaultPolicy: "ask",
  },
  {
    id: "pipeline.task.update",
    group: "Pipelines",
    label: "Edit a pipeline task",
    description: "Update the allowed fields of an unlaunched pipeline ticket.",
    risk: "persistent",
    defaultPolicy: "ask",
  },
  {
    id: "pipeline.task.ready",
    group: "Pipelines",
    label: "Change pipeline readiness",
    description: "Mark a pipeline ticket ready or return it to draft.",
    risk: "reversible",
    defaultPolicy: "ask",
  },
  {
    id: "pipeline.task.launch",
    group: "Pipelines",
    label: "Launch a pipeline task now",
    description: "Start a ticket immediately, subject to server concurrency and dependency checks.",
    risk: "compute",
    defaultPolicy: "ask",
  },
  {
    id: "pipeline.task.reset",
    group: "Pipelines",
    label: "Reset a pipeline task",
    description: "Cancel its active run tree and stage the ticket to start again.",
    risk: "destructive",
    defaultPolicy: "ask",
  },
  {
    id: "pipeline.task.close",
    group: "Pipelines",
    label: "Close a pipeline task",
    description: "Cancel active work and file the ticket as abandoned.",
    risk: "destructive",
    defaultPolicy: "ask",
  },
  {
    id: "pipeline.task.delete",
    group: "Pipelines",
    label: "Delete a pipeline task",
    description: "Permanently remove an inactive pipeline ticket.",
    risk: "destructive",
    defaultPolicy: "ask",
  },
  {
    id: "run.launch",
    group: "Runs",
    label: "Launch a bot run",
    description: "Resolve a catalog bot and launch it with bounded string variables.",
    risk: "compute",
    defaultPolicy: "ask",
  },
  {
    id: "run.pause",
    group: "Runs",
    label: "Pause a run",
    description: "Request a resumable pause at the next safe boundary.",
    risk: "reversible",
    defaultPolicy: "ask",
  },
  {
    id: "run.resume",
    group: "Runs",
    label: "Resume a run",
    description: "Resume a paused or resumable run using its saved checkpoint.",
    risk: "compute",
    defaultPolicy: "ask",
  },
  {
    id: "run.cancel",
    group: "Runs",
    label: "Cancel a run",
    description: "Stop a run and leave it in a resumable cancelled state when supported.",
    risk: "destructive",
    defaultPolicy: "ask",
  },
  {
    id: "run.rename",
    group: "Runs",
    label: "Rename a run",
    description: "Change the friendly display name of an existing run.",
    risk: "reversible",
    defaultPolicy: "ask",
  },
  {
    id: "run.delete",
    group: "Runs",
    label: "Delete a run",
    description: "Permanently remove a run and all of its events and artifacts.",
    risk: "destructive",
    defaultPolicy: "ask",
  },
  {
    id: "dispatcher.start",
    group: "Dispatcher",
    label: "Start the dispatcher",
    description: "Start processing eligible board work with the saved configuration.",
    risk: "compute",
    defaultPolicy: "ask",
  },
  {
    id: "dispatcher.pause",
    group: "Dispatcher",
    label: "Pause the dispatcher",
    description: "Pause new dispatcher admissions without deleting configuration.",
    risk: "reversible",
    defaultPolicy: "ask",
  },
  {
    id: "dispatcher.resume",
    group: "Dispatcher",
    label: "Resume the dispatcher",
    description: "Resume admissions on a paused dispatcher.",
    risk: "compute",
    defaultPolicy: "ask",
  },
  {
    id: "dispatcher.stop",
    group: "Dispatcher",
    label: "Stop the dispatcher",
    description: "Stop the dispatcher service for this workspace.",
    risk: "destructive",
    defaultPolicy: "ask",
  },
  {
    id: "bot.create",
    group: "Bots",
    label: "Create a bot bundle",
    description: "Scaffold a new local bot bundle from bounded metadata and instructions.",
    risk: "persistent",
    defaultPolicy: "ask",
  },
  {
    id: "bot.install",
    group: "Bots",
    label: "Install a bot bundle",
    description: "Install a bot from an explicit Git URL or local source supported by the server.",
    risk: "persistent",
    defaultPolicy: "ask",
  },
  {
    id: "plugin.enable",
    group: "Plugins",
    label: "Enable a plugin",
    description: "Enable an already installed plugin and its declared contributions.",
    risk: "compute",
    defaultPolicy: "ask",
  },
  {
    id: "plugin.disable",
    group: "Plugins",
    label: "Disable a plugin",
    description: "Disable an installed plugin without uninstalling it.",
    risk: "reversible",
    defaultPolicy: "ask",
  },
  {
    id: "plugin.install",
    group: "Plugins",
    label: "Install a plugin",
    description: "Install plugin code from an explicit Git URL or local source; server permissions still apply.",
    risk: "compute",
    defaultPolicy: "ask",
  },
  {
    id: "plugin.uninstall",
    group: "Plugins",
    label: "Uninstall a plugin",
    description: "Remove a non-builtin plugin from this Iterion installation.",
    risk: "destructive",
    defaultPolicy: "ask",
  },
] as const;

const ASSISTANT_ACTION_IDS = new Set<string>(
  ASSISTANT_ACTIONS.map((action) => action.id),
);

export function isAssistantActionId(value: unknown): value is AssistantActionId {
  return typeof value === "string" && ASSISTANT_ACTION_IDS.has(value);
}

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

export type AssistantActionIntent = "suggested" | "explicit";

export interface AssistantActionRequest {
  key: string;
  id: AssistantActionId;
  intent: AssistantActionIntent;
  args: Record<string, unknown>;
}

export function parseAssistantActionRequests(
  value: unknown,
  keyPrefix: string,
): AssistantActionRequest[] {
  let raw = value;
  if (typeof raw === "string") {
    try {
      raw = JSON.parse(raw);
    } catch {
      return [];
    }
  }
  if (!Array.isArray(raw)) return [];
  return raw.slice(0, 8).flatMap((entry, index) => {
    if (!entry || typeof entry !== "object" || Array.isArray(entry)) return [];
    const item = entry as Record<string, unknown>;
    if (!isAssistantActionId(item.id)) return [];
    const args =
      item.args && typeof item.args === "object" && !Array.isArray(item.args)
        ? (item.args as Record<string, unknown>)
        : {};
    return [
      {
        key: `${keyPrefix}:${index}`,
        id: item.id,
        intent: item.intent === "explicit" ? "explicit" : "suggested",
        args,
      },
    ];
  });
}

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
