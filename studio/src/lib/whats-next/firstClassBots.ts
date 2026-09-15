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
//
// whats-next v2 note: the bot is ONE conversational agent (`nexie`) in
// a chat loop — the transcript is Nexie's narration + replies and the
// operator's messages, so the map is tiny (banner for the working
// agent, a single free-text human turn). The v1 form state machine
// (roadmap cards, dispatch checkbox pickers, ask_continue radio) and
// its per-node dynamic forms are gone.

import type { FormSpec } from "./questionForm";

export type WhatsNextNodeKind = "banner" | "human" | "silent";

export interface WhatsNextNodeMapEntry {
  kind: WhatsNextNodeKind;
  // Label shown in the progress banner ("Nexie is working…").
  label?: string;
  // For "banner" entries: pluck this field from the node output as the
  // collapsed summary text. Optional — if absent, the banner closes
  // without a summary line.
  summaryField?: string;
  // For "human" entries: the assistant-side prompt displayed above the
  // user input. Leave unset to use the runtime-resolved `instructions:`
  // prompt from the event (whats-next v2 relies on this — the chat
  // node's instructions ARE Nexie's reply).
  prompt?: string;
  // For "human" entries with custom actions (e.g. approve/request_revision).
  actions?: ReadonlyArray<"approve" | "request_revision">;
  // For "human" entries: the schema field name where the user's typed
  // text lands (chat → "message").
  textField?: string;
  // For "human" entries with approve/reject buttons: the schema field
  // name for the boolean outcome.
  approvedField?: string;
  // For "human" entries: a rich form specification. When set, the
  // HumanChatTurn renders the form via WizardForm and the form answers
  // are submitted as-is (question.id IS the answer key).
  form?: FormSpec;
  // For "human" entries whose answer isn't a plain text field:
  // synthesise the AnsweredTurn label from the full answers map.
  formatAnswer?: (
    answers: Record<string, unknown>,
    upstream?: ReadonlyArray<unknown>,
  ) => string;
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
  // Optional upfront form rendered by SessionLauncher instead of the
  // bare Start button. Its (single-question) answer is written into
  // the launch vars under `seedVar` — the bot reads it as the first
  // operator message. No auto-submit machinery: the seed is a var.
  launcherForm?: FormSpec;
  // Launch var receiving the launcher form's answer text (and the
  // always-on composer's re-seed text). whats-next → "initial_message".
  seedVar?: string;
  editor?: {
    context: boolean;
    proposals: boolean;
  };
  nodeMap: Readonly<Record<string, WhatsNextNodeMapEntry>>;
}

// The launcher's focus presets are just canned seed messages — the
// operator can equally type their own ("Other"). Values are what
// Nexie receives verbatim as the first message.
const seedMessageForm: FormSpec = {
  questions: [
    {
      id: "seed",
      kind: "radio",
      label: "What do you want to look at?",
      description:
        "Pick a starting point or type your own — this is just the first message of the conversation.",
      options: [
        {
          value: "Fais le point sur le board et recommande la prochaine action.",
          label: "What's next? (board + recommendation)",
          description: "Board state, quick wins, Nexie's pick.",
        },
        {
          value:
            "Quels tickets sont dispatchables maintenant ? Analyse et recommande lesquels pousser, puis attends ma confirmation.",
          label: "Dispatch existing board items",
          description: "Shortlist + recommendation before anything moves.",
        },
        {
          value:
            "Surveye le repo et propose une roadmap courte (long terme / court terme / prochaine action) avec les bots à utiliser.",
          label: "Survey the repo & propose a roadmap",
          description: "Read-only survey, then a structured proposal.",
        },
        {
          value:
            "Passe le backlog en revue : lesquels sont encore pertinents par rapport au code actuel ? Propose un nettoyage.",
          label: "Clean up the board",
          description: "Relevance check against code + git history.",
        },
      ],
      allow_other: true,
    },
  ],
  submitLabel: "Start the conversation",
};

export const FIRST_CLASS_BOTS: Readonly<Record<string, FirstClassBot>> = {
  "whats-next": {
    id: "whats-next",
    label: "What's Next",
    description:
      "Nexie, your co-CTO, in a standing conversation: board intelligence and recommendations, ticket creation and curation (with relevance checks against the code), and dispatch — all in plain language.",
    workflowPath: "bots/whats-next/main.bot",
    // Studio launches scope to the server's current work_dir, so the
    // bot's `workspace_dir` var resolves to the same path via its
    // `${PROJECT_DIR}` default. No launcher vars needed.
    launcherVars: [],
    launcherForm: seedMessageForm,
    seedVar: "initial_message",
    nodeMap: {
      seed: { kind: "silent" },
      gate: { kind: "silent" },
      nexie: {
        kind: "banner",
        label: "Nexie is working",
      },
      // The chat pause: Nexie's reply arrives as the runtime-resolved
      // instructions (rendered as the assistant bubble), the operator
      // answers through the unified composer. No `prompt` here — the
      // instructions must win.
      chat: {
        kind: "human",
        textField: "message",
      },
    },
  },
};

export const DEFAULT_WHATS_NEXT_BOT_ID = "whats-next";

export function getFirstClassBot(id: string): FirstClassBot | null {
  return FIRST_CLASS_BOTS[id] ?? null;
}
