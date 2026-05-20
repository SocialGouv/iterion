// First-class bot registry — bots that get a dedicated /whats-next
// experience instead of being launched generically through LaunchView.
//
// v0 hard-codes the single whats-next entry. When a second first-class
// bot lands, promote this registry to a manifest-driven discovery (or
// a server-side endpoint), and replace the const with a fetch.
//
// `nodeMap` describes how each node id of the workflow renders in the
// WhatsNext chat. The keys must match the `.bot` source — a rename there
// without updating the map silently drops the node from the chat.

import type { FormSpec } from "./questionForm";

export type WhatsNextNodeKind =
  | "banner"
  | "human"
  | "silent"
  | "issues-summary"
  | "roadmap";

export interface WhatsNextNodeMapEntry {
  kind: WhatsNextNodeKind;
  // Label shown in the progress banner ("Surveying repository…").
  label?: string;
  // For agent nodes whose output should be promoted to a typed card
  // after the banner closes. Each kind has its own renderer.
  followCardKind?: "roadmap" | "issuesSummary" | "survey";
  // For "banner" entries: pluck this field from the node output as the
  // collapsed summary text. Optional — if absent, the banner closes
  // without a summary line.
  summaryField?: string;
  // For "human" entries: the assistant-side prompt displayed above the
  // user input. Static for v0; future iterations may pull from the
  // node's `instructions:` block.
  prompt?: string;
  // For "human" entries with custom actions (e.g. human_review's
  // approve/request_revision). Default: free-text submit.
  actions?: ReadonlyArray<"approve" | "request_revision">;
  // For "human" entries: the schema field name where the user's typed
  // text lands. ask_priorities → "context"; human_review → "feedback".
  // Required for kind "human" when no `form` is set (the legacy
  // textarea-only path).
  textField?: string;
  // For "human" entries with approve/reject buttons: the schema field
  // name for the boolean outcome. human_review → "approved". Optional —
  // free-text-only turns leave this unset.
  approvedField?: string;
  // For "human" entries: a rich form specification (radio / checkbox /
  // select / free_text, with optional "Other"). When set, the
  // HumanChatTurn renders the form via WizardForm and the form
  // answers are submitted as-is (question.id IS the answer key). When
  // unset, the legacy single-textarea + optional approve/reject UI
  // kicks in.
  form?: FormSpec;
}

export interface FirstClassBot {
  id: string;
  label: string;
  description: string;
  // Path relative to the server's work_dir. Resolved at launch time.
  workflowPath: string;
  // Vars to expose in the SessionLauncher with pre-fill rules.
  launcherVars: ReadonlyArray<{
    name: string;
    label: string;
    defaultFrom?: "work_dir";
  }>;
  nodeMap: Readonly<Record<string, WhatsNextNodeMapEntry>>;
}

export const FIRST_CLASS_BOTS: Readonly<Record<string, FirstClassBot>> = {
  "whats-next": {
    id: "whats-next",
    label: "What's Next",
    description:
      "Decide what to push on the board next. Iterion surveys the repo, drafts a roadmap, you approve, and it materialises as kanban issues the dispatcher can dispatch.",
    workflowPath: "examples/whats-next/main.bot",
    launcherVars: [
      { name: "workspace_dir", label: "Workspace", defaultFrom: "work_dir" },
    ],
    nodeMap: {
      explore: {
        kind: "banner",
        label: "Surveying the repository",
        followCardKind: "survey",
      },
      ask_priorities: {
        kind: "human",
        prompt: "What matters right now?",
        textField: "context",
        // Rich form: radio with a few common focuses plus an "Other"
        // free-text fallback. The chosen value lands in `context`
        // either as one of the option strings or as the typed text —
        // both satisfy the priorities_output schema (context: string).
        form: {
          questions: [
            {
              id: "context",
              kind: "radio",
              label: "What matters right now?",
              description:
                "Pick a focus or type your own. Iterion will draft a roadmap based on this.",
              options: [
                {
                  value: "Ship a specific feature",
                  label: "Ship a specific feature",
                  description: "There's a feature I want delivered next.",
                },
                {
                  value: "Fix bugs / pay down tech debt",
                  label: "Fix bugs / pay down tech debt",
                  description: "Stabilise before adding more.",
                },
                {
                  value: "Explore — surface what's important",
                  label: "Explore — surface what's important",
                  description: "I want iterion to propose, not me.",
                },
                {
                  value: "Polish UX / docs",
                  label: "Polish UX / docs",
                  description: "Smooth the rough edges, ship docs.",
                },
              ],
              allow_other: true,
            },
          ],
        },
      },
      propose_roadmap: {
        kind: "banner",
        label: "Drafting the roadmap",
        followCardKind: "roadmap",
      },
      carry_roadmap: { kind: "silent" },
      human_review: {
        kind: "human",
        prompt: "Review the proposed roadmap.",
        actions: ["approve", "request_revision"],
        textField: "feedback",
        approvedField: "approved",
      },
      revise_roadmap: {
        kind: "banner",
        label: "Revising the roadmap",
        followCardKind: "roadmap",
      },
      emit_action: {
        kind: "banner",
        label: "Creating kanban issues",
        followCardKind: "issuesSummary",
      },
      // Post-emit triage loop: ask the operator which freshly-created
      // issues to dispatch, hand them off to assign_to_bots, then
      // loop on ask_continue / triage_board until the operator
      // picks action=done.
      ask_which_to_process: {
        kind: "human",
        prompt:
          "Pick which issues to push from backlog to ready (the dispatcher will pick them up).",
        textField: "selected_issue_ids",
        form: {
          questions: [
            {
              id: "selected_issue_ids",
              kind: "free_text",
              label: "Issue IDs to dispatch",
              description:
                'Comma- or newline-separated IDs (short prefixes ok), or "all", or leave empty to keep everything in backlog.',
              placeholder: 'e.g. "abc12345, def67890" or "all"',
              rows: 3,
              required: false,
            },
            {
              id: "note",
              kind: "free_text",
              label: "Note (optional)",
              description:
                "Why this selection? Helps the bot reason about edge cases.",
              placeholder: "Optional — e.g. 'skip long_term, only ship next_action'",
              rows: 2,
              required: false,
            },
          ],
          submitLabel: "Dispatch",
        },
      },
      assign_to_bots: {
        kind: "banner",
        label: "Moving issues to ready",
        summaryField: "summary",
      },
      ask_continue: {
        kind: "human",
        prompt: "What's next on the board?",
        textField: "free_text",
        form: {
          questions: [
            {
              id: "action",
              kind: "radio",
              label: "Pick an action",
              options: [
                {
                  value: "add_ticket",
                  label: "Add a ticket",
                  description: "Create a new ticket on the board.",
                },
                {
                  value: "modify_ticket",
                  label: "Modify a ticket",
                  description:
                    "Re-assign, re-label, move, or close existing tickets.",
                },
                {
                  value: "dispatch_more",
                  label: "Dispatch more",
                  description:
                    "Push more backlog tickets to ready so the dispatcher picks them up.",
                },
                {
                  value: "done",
                  label: "I'm done",
                  description: "End this session.",
                },
              ],
              required: true,
            },
            {
              id: "free_text",
              kind: "free_text",
              label: "Describe what you want",
              description:
                'Optional for "I\'m done". Required for the other actions — be specific (which ticket, what change, what criteria).',
              placeholder: "What should the triage agent do?",
              rows: 3,
              required: false,
            },
          ],
          submitLabel: "Continue",
        },
      },
      // Deterministic projection of ask_continue.action → is_done.
      // Silent: the operator doesn't need to see it.
      derive_continue: { kind: "silent" },
      triage_board: {
        kind: "banner",
        label: "Updating the board",
        summaryField: "board_summary",
      },
    },
  },
};

export function getFirstClassBot(id: string): FirstClassBot | null {
  return FIRST_CLASS_BOTS[id] ?? null;
}

export const DEFAULT_WHATS_NEXT_BOT_ID = "whats-next";
