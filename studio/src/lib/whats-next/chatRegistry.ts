// The chat registry — DISCOVERED from bot manifests, not declared here.
//
// This file replaces the const `FIRST_CLASS_BOTS` map, whose own comment
// asked for exactly this ("promote this registry to a manifest-driven
// discovery … and replace the const with a fetch"). A bot id baked into the
// product is the studio-side twin of the rule CLAUDE.md states for the
// engine: adding a second conversational bot must cost a bundle, not a
// release.
//
// What a manifest CANNOT carry is a function, so `formatAnswer` — the only
// such field on the legacy shape — is absent from a discovered entry. Nothing
// shipped uses it: both chat bots answer with a plain text field. If a future
// bot needs derived answer labels, the answer is another declarative field,
// not a per-bot function smuggled back into the studio.

import type {
  BotChatNode,
  BotChatSurface,
  BotEntry,
} from "@/api/bots";
import type { FirstClassBot, WhatsNextNodeMapEntry } from "./firstClassBots";
import type { FormSpec } from "./questionForm";

/** A bot entry carrying a `chat:` block is a conversational bot. */
export function isChatBot(entry: BotEntry): boolean {
  return !!entry.chat && hasHumanTurn(entry.chat);
}

// A chat surface with no human turn could never be answered. The Go side
// rejects that at manifest load, so this is defence in depth for a payload
// from an older/newer server rather than a case we expect to hit — but the
// failure it prevents (a chat window that swallows every message) is silent
// enough to be worth the four lines.
function hasHumanTurn(chat: BotChatSurface): boolean {
  return Object.values(chat.nodes ?? {}).some((n) => n.kind === "human");
}

/** Convert one discovered bot entry into the shape the chat surfaces
 *  already consume. Returns null when the entry declares no usable chat
 *  surface, so callers can `.filter(Boolean)` a whole listing. */
export function chatBotFromEntry(entry: BotEntry): FirstClassBot | null {
  const chat = entry.chat;
  if (!chat || !isChatBot(entry)) return null;

  // The workflow path is what the launcher hands the run API. `rel_path` is
  // the server's workspace-relative form and the one a launch can resolve;
  // the absolute `path` is the fallback for a server that did not compute
  // one. A bundle's entry points at its directory, so append main.bot.
  const workflowPath = workflowPathFor(entry);
  if (!workflowPath) return null;

  const nodeMap: Record<string, WhatsNextNodeMapEntry> = {};
  for (const [id, node] of Object.entries(chat.nodes ?? {})) {
    nodeMap[id] = nodeMapEntry(node);
  }

  return {
    id: entry.name,
    label: chat.label || entry.display_name || entry.name,
    description: chat.description || entry.description || "",
    workflowPath,
    launcherVars: (chat.launcher_vars ?? []).map((v) => ({
      name: v.name,
      label: v.label || v.name,
      // Narrow to the one source the launcher implements. An unknown value
      // becomes "no pre-fill" rather than a crash: a bundle authored against
      // a newer studio must still render here.
      ...(v.default_from === "work_dir" ? { defaultFrom: "work_dir" as const } : {}),
    })),
    ...(chat.launcher ? { launcherForm: formSpecFrom(chat.launcher) } : {}),
    ...(chat.seed_var ? { seedVar: chat.seed_var } : {}),
    nodeMap,
  };
}

function nodeMapEntry(node: BotChatNode): WhatsNextNodeMapEntry {
  const out: WhatsNextNodeMapEntry = { kind: node.kind };
  if (node.label) out.label = node.label;
  if (node.summary_field) out.summaryField = node.summary_field;
  if (node.prompt) out.prompt = node.prompt;
  if (node.text_field) out.textField = node.text_field;
  if (node.approved_field) out.approvedField = node.approved_field;
  return out;
}

// The launcher form is always ONE radio question: it is a canned opener, and
// the answer goes verbatim into seedVar. Keeping it single-question is what
// lets a manifest express it at all — a general form builder in YAML would be
// a second DSL.
function formSpecFrom(launcher: NonNullable<BotChatSurface["launcher"]>): FormSpec {
  return {
    questions: [
      {
        id: "seed",
        kind: "radio",
        label: launcher.prompt || "What do you want to look at?",
        ...(launcher.description ? { description: launcher.description } : {}),
        options: (launcher.presets ?? []).map((p) => ({
          value: p.value,
          label: p.label || p.value,
          ...(p.description ? { description: p.description } : {}),
        })),
        // Default TRUE — a canned list with no way out turns a conversation
        // into a menu. The Go normalizer defaults it the same way, so this
        // only covers a payload that predates the field.
        allow_other: launcher.allow_other !== false,
      },
    ],
    submitLabel: launcher.submit_label || "Start the conversation",
  };
}

function workflowPathFor(entry: BotEntry): string {
  const base = (entry.rel_path || entry.path || "").replace(/\/+$/, "");
  if (!base) return "";
  if (base.endsWith(".bot") || base.endsWith(".botz")) return base;
  return `${base}/main.bot`;
}

/** Build the registry from a bot listing, keyed by bot id. */
export function chatRegistryFrom(
  entries: readonly BotEntry[],
): Record<string, FirstClassBot> {
  const out: Record<string, FirstClassBot> = {};
  for (const entry of entries) {
    // A bot the operator disabled in the Catalog manager is not offered as a
    // chat surface either — one visibility decision, not two.
    if (entry.enabled === false) continue;
    const bot = chatBotFromEntry(entry);
    if (bot) out[bot.id] = bot;
  }
  return out;
}
