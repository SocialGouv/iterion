// The dock's conversations: several at once, each its own run.
//
// Until now the studio held ONE assistant session. That is fine while the
// assistant answers about the page you are on, and wrong as soon as you want
// to keep a thread going — asking about a run while a workflow you are
// drafting waits meant losing one of them.
//
// A conversation is deliberately thin: an id, which bot answers, and WHERE it
// started. It does NOT hold the run id — the session hook discovers that from
// the store, and duplicating it here would create a second source of truth
// that goes stale the moment a session is cancelled or re-seeded elsewhere.
//
// `origin` is the typed reference of the page the conversation was opened
// from ("view/board", "run/019f…"). It is what lets the operator get back to
// what they were talking about after switching tabs — the reverse of the page
// context the dock already sends the bot.

import { readStringFlag, writeStringFlag } from "@/lib/localStorageFlag";

export const CONVERSATIONS_KEY = "iterion.chatDock.conversations";
export const ACTIVE_CONVERSATION_KEY = "iterion.chatDock.activeConversation";

// A ceiling, not a preference. Every open conversation is a live session with
// its own polling, so an unbounded strip is a way to quietly melt the browser.
export const MAX_CONVERSATIONS = 8;

export interface Conversation {
  id: string;
  botId: string;
  /** Typed reference of the page it was opened from, e.g. "run/019f…". */
  origin?: string;
  /** Human label for that reference, e.g. "Board". */
  originLabel?: string;
  /**
   * Opened by the operator in THIS browser session, and not yet launched.
   *
   * Such a conversation must not attach to an existing run: discovery is keyed
   * on (bot, scope), so it would be handed the run another tab is already
   * showing — which is exactly what "click +, see the old conversation" was.
   * Cleared once it has a run of its own; a conversation restored from
   * localStorage is not fresh, so the operator who closed their tab mid-run
   * still gets it back.
   */
  fresh?: boolean;
}

export function newConversationId(): string {
  if (typeof crypto !== "undefined" && "randomUUID" in crypto) {
    return crypto.randomUUID();
  }
  return `c-${Math.random().toString(36).slice(2)}-${Date.now()}`;
}

function isConversation(v: unknown): v is Conversation {
  if (!v || typeof v !== "object") return false;
  const c = v as Partial<Conversation>;
  return typeof c.id === "string" && c.id !== "" && typeof c.botId === "string";
}

/**
 * readConversations returns the persisted list, dropping anything it cannot
 * make sense of. A corrupt entry costs its own conversation, never the strip:
 * the dock is a helper, and it must not be possible to lock yourself out of it
 * by hand-editing localStorage or by a shape change between builds.
 */
export function readConversations(): Conversation[] {
  const raw = readStringFlag(CONVERSATIONS_KEY, "");
  if (!raw) return [];
  try {
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    return parsed.filter(isConversation).slice(0, MAX_CONVERSATIONS);
  } catch {
    return [];
  }
}

export function writeConversations(list: readonly Conversation[]): void {
  writeStringFlag(
    CONVERSATIONS_KEY,
    JSON.stringify(list.slice(0, MAX_CONVERSATIONS)),
  );
}

export function readActiveConversation(): string {
  return readStringFlag(ACTIVE_CONVERSATION_KEY, "");
}

export function writeActiveConversation(id: string): void {
  writeStringFlag(ACTIVE_CONVERSATION_KEY, id);
}

/** Adds a conversation, refusing to exceed the ceiling. */
export function addConversation(
  list: readonly Conversation[],
  next: Conversation,
): Conversation[] {
  if (list.length >= MAX_CONVERSATIONS) return [...list];
  return [...list, next];
}

/**
 * closeConversation removes one and says which should take its place.
 *
 * The neighbour, not the first tab: closing the third of five should leave you
 * looking at its neighbour, the way every tab strip behaves. Returns a null
 * active id when nothing is left, which the caller reads as "back to the
 * empty state" rather than as an error.
 */
export function closeConversation(
  list: readonly Conversation[],
  id: string,
  activeId: string,
): { list: Conversation[]; activeId: string | null } {
  const index = list.findIndex((c) => c.id === id);
  if (index === -1) return { list: [...list], activeId };
  const next = list.filter((c) => c.id !== id);
  if (next.length === 0) return { list: next, activeId: null };
  if (id !== activeId) return { list: next, activeId };
  const neighbour = next[Math.min(index, next.length - 1)];
  return { list: next, activeId: neighbour?.id ?? null };
}

/**
 * resolveActive picks which conversation is on screen.
 *
 * A persisted id that no longer exists (closed in another tab, dropped by a
 * shape change) falls back to the first rather than leaving the dock blank.
 */
export function resolveActive(
  list: readonly Conversation[],
  activeId: string,
): Conversation | null {
  if (list.length === 0) return null;
  return list.find((c) => c.id === activeId) ?? list[0] ?? null;
}
