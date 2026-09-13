// The dock's conversations: several at once, each its own run.
//
// Until now the studio held ONE assistant session. That is fine while the
// assistant answers about the page you are on, and wrong as soon as you want
// to keep a thread going — asking about a run while a workflow you are
// drafting waits meant losing one of them.
//
// A conversation is thin: an id, which bot answers, WHERE its first message
// started, and the run it owns. That last one was left out at first — the
// session hook can
// discover a run — and it had to come back: discovery is keyed on the BOT, so
// conversations sharing one were handed each other's run.
//
// `origin` is the typed reference of the page where the first accepted message
// was sent ("view/board", "run/019f…"). It is what lets the operator get back to
// what they were talking about after switching tabs — the reverse of the page
// context the dock already sends the bot.

import { readStringFlag, writeStringFlag } from "@/lib/localStorageFlag";
import { isSafeStudioHref } from "@/lib/chatDock/routeReference";
import { scopePrefix } from "@/lib/scope";
import type { RunEvent } from "@/api/runs";

export const CONVERSATIONS_KEY = "iterion.chatDock.conversations";
export const ACTIVE_CONVERSATION_KEY = "iterion.chatDock.activeConversation";

// Workspace panes share one browser origin, so raw localStorage keys would
// make one project's run ids visible to every other project. Keep the legacy
// keys for the unscoped Studio and append the stable project id in a pane.
export function conversationStorageKey(key: string): string {
  const scope = scopePrefix();
  return scope ? `${key}:${scope.slice(3)}` : key;
}

// A ceiling, not a preference. Every open conversation is a live session with
// its own polling, so an unbounded strip is a way to quietly melt the browser.
export const MAX_CONVERSATIONS = 8;

export type ConversationContextState =
  | "pending"
  | "anchored"
  | "disabled"
  | "unknown";

export interface ConversationAnchor {
  ref: string;
  label: string;
  /** Exact Studio location, including the query string when it matters. */
  href?: string;
}

export interface Conversation {
  id: string;
  botId: string;
  /** Typed reference captured for the first accepted message. */
  origin?: string;
  /** Human label for that reference, e.g. "Board". */
  originLabel?: string;
  /** Exact in-Studio destination for the immutable first-message anchor. */
  originHref?: string;
  /**
   * Lifecycle of the implicit page context.
   *
   * Missing is the legacy shape. Legacy entries are interpreted from their
   * run/transcript and migrated without ever borrowing the route currently on
   * screen.
   */
  contextState?: ConversationContextState;
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
  /**
   * The run this conversation owns, once it has launched one.
   *
   * The model started without it on purpose — "the session hook discovers that
   * from the store" — and that was wrong. Discovery answers "the latest live
   * run for this BOT", so the moment two conversations share a bot they take
   * each other's run: switching tabs remounted a session, it re-discovered,
   * and both ended up showing the same thread while the other was lost.
   *
   * Owning the id is what makes a conversation a conversation rather than a
   * view onto whatever ran last.
   */
  runId?: string;
  /** Host-owned correlation for a Copi conversation opened by a handoff. */
  workspaceHandoffId?: string;
  /**
   * Latest finalized automatic run-watch turn the operator has actually seen.
   *
   * A chat run normally rests in `paused_waiting_human`, before and after a
   * watch fires. The status therefore cannot say whether the pause contains
   * an old conversational answer or a new automatic diagnosis. Persisting the
   * result event sequence gives the dock a real unread cursor across reloads.
   */
  lastReadWatchResultSeq?: number;
}

export interface ConversationRuntimeSnapshot {
  runId: string | null;
  runStatus: string | null | undefined;
  latestWatchResultSeq?: number | null;
}

/**
 * One badge entry per dock tab whose OWN run is parked on a human gate.
 * Message count and read state are intentionally irrelevant: for a chat bot,
 * this is the ordinary "ready for your next message" resting state.
 */
export function collectWaitingConversationIds(
  conversations: readonly Conversation[],
  runtimeFor: (conversationId: string) => ConversationRuntimeSnapshot,
): Set<string> {
  const waiting = new Set<string>();
  for (const conversation of conversations) {
    const runtime = runtimeFor(conversation.id);
    if (
      conversation.runId &&
      runtime.runId === conversation.runId &&
      runtime.runStatus === "paused_waiting_human"
    ) {
      waiting.add(conversation.id);
    }
  }
  return waiting;
}

/**
 * Return the latest finalized assistant reply caused by a host run-watch.
 *
 * The host resumes the manifest chat node by recording `host_event`; the
 * automatic turn is only publishable once that SAME human node asks for input
 * again. Matching the node ids avoids treating an ask_user pause inside the
 * diagnostic turn as a completed alert.
 */
export function latestWatchResultSeq(
  events: readonly RunEvent[],
): number | null {
  const watchStarts: Array<{ seq: number; nodeId: string }> = [];
  let latest: number | null = null;
  for (const event of events) {
    if (event.type === "human_answers_recorded") {
      const hostEvent = event.data?.answers?.host_event;
      if (
        event.node_id &&
        hostEvent &&
        typeof hostEvent === "object" &&
        !Array.isArray(hostEvent) &&
        ((hostEvent as Record<string, unknown>).kind === "assistant-watch-event" ||
          (hostEvent as Record<string, unknown>).kind ===
            "workspace-handoff-completed")
      ) {
        watchStarts.push({ seq: event.seq, nodeId: event.node_id });
      }
      continue;
    }
    if (event.type !== "human_input_requested" || !event.node_id) continue;
    for (const start of watchStarts) {
      if (event.seq > start.seq && event.node_id === start.nodeId) {
        latest = Math.max(latest ?? event.seq, event.seq);
      }
    }
  }
  return latest;
}

/** One unread badge per conversation carrying a finalized watch diagnosis. */
export function collectUnreadWatchConversationIds(
  conversations: readonly Conversation[],
  runtimeFor: (conversationId: string) => ConversationRuntimeSnapshot,
): Set<string> {
  const unread = new Set<string>();
  for (const conversation of conversations) {
    const runtime = runtimeFor(conversation.id);
    const latest = runtime.latestWatchResultSeq ?? null;
    if (
      conversation.runId &&
      runtime.runId === conversation.runId &&
      latest !== null &&
      latest > (conversation.lastReadWatchResultSeq ?? -1)
    ) {
      unread.add(conversation.id);
    }
  }
  return unread;
}

/** Advance, never rewind, a conversation's persisted watch-read cursor. */
export function markConversationWatchResultRead(
  list: readonly Conversation[],
  id: string,
  seq: number,
): Conversation[] {
  if (!Number.isSafeInteger(seq) || seq < 0) return list.map((c) => ({ ...c }));
  return list.map((conversation) =>
    conversation.id === id &&
    seq > (conversation.lastReadWatchResultSeq ?? -1)
      ? { ...conversation, lastReadWatchResultSeq: seq }
      : { ...conversation },
  );
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
  const optionalString = (value: unknown) =>
    value === undefined || typeof value === "string";
  const optionalStudioHref = (value: unknown) =>
    value === undefined || isSafeStudioHref(value);
  return (
    typeof c.id === "string" &&
    c.id !== "" &&
    typeof c.botId === "string" &&
    optionalString(c.origin) &&
    optionalString(c.originLabel) &&
    optionalStudioHref(c.originHref) &&
    optionalString(c.runId) &&
    optionalString(c.workspaceHandoffId) &&
    (c.lastReadWatchResultSeq === undefined ||
      (Number.isSafeInteger(c.lastReadWatchResultSeq) &&
        c.lastReadWatchResultSeq >= 0)) &&
    (c.contextState === undefined ||
      c.contextState === "pending" ||
      c.contextState === "anchored" ||
      c.contextState === "disabled" ||
      c.contextState === "unknown") &&
    (c.fresh === undefined || typeof c.fresh === "boolean")
  );
}

/**
 * Effective state while legacy records are being migrated.
 *
 * An old unlaunched tab may already carry `origin`, because the previous UI
 * stamped it when "+" was clicked. It is still pending: the first send is
 * authoritative and may happen from another page. A started legacy tab may
 * use its old origin as a temporary fallback until the transcript recovers
 * the first machine-generated page pointer.
 */
export function conversationContextState(
  conversation: Conversation,
  hasTranscript: boolean,
): ConversationContextState {
  // An explicit pending state remains pending until the opening send commits
  // its anchor (or transcript recovery finalises it). createRun publishes the
  // run id before launch hydration and before ChatDock's awaited send returns;
  // treating that short-lived run id as proof of a missing anchor flashes
  // "Original context unavailable" during every new conversation.
  if (conversation.contextState) return conversation.contextState;
  if (hasTranscript) {
    return conversation.origin ? "anchored" : "unknown";
  }
  // Legacy records may have stamped their creation-time origin before the
  // first send. It is a useful temporary anchor while their transcript loads,
  // but a run id alone says only that startup is in flight, not that the
  // opening context was unreadable.
  if (conversation.runId && conversation.origin) return "anchored";
  return "pending";
}

/** First-write-wins for normal sends; legacy (unmarked) origins may migrate. */
export function anchorConversation(
  list: readonly Conversation[],
  id: string,
  anchor: ConversationAnchor,
): Conversation[] {
  return list.map((conversation) => {
    if (conversation.id !== id) return { ...conversation };
    if (conversation.contextState === "anchored") return { ...conversation };
    if (conversation.contextState === "disabled") return { ...conversation };
    return {
      ...conversation,
      origin: anchor.ref,
      originLabel: anchor.label,
      ...(isSafeStudioHref(anchor.href)
        ? { originHref: anchor.href }
        : {}),
      contextState: "anchored",
    };
  });
}

/** Finalise a legacy conversation whose opening pointer cannot be recovered. */
export function markConversationContextUnknown(
  list: readonly Conversation[],
  id: string,
): Conversation[] {
  return list.map((conversation) => {
    if (
      conversation.id !== id ||
      (conversation.contextState !== undefined &&
        conversation.contextState !== "pending")
    ) {
      return { ...conversation };
    }
    return { ...conversation, contextState: "unknown" };
  });
}

/**
 * Opt out before the first send. This is conversation-scoped: removing the
 * candidate from one empty tab must not affect another tab on the same page.
 */
export function setConversationContextEnabled(
  list: readonly Conversation[],
  id: string,
  enabled: boolean,
): Conversation[] {
  return list.map((conversation) => {
    if (conversation.id !== id) return { ...conversation };
    if (
      conversation.contextState === "anchored" ||
      conversation.contextState === "unknown" ||
      conversation.runId
    ) {
      return { ...conversation };
    }
    const next = { ...conversation };
    delete next.origin;
    delete next.originLabel;
    delete next.originHref;
    next.contextState = enabled ? "pending" : "disabled";
    return next;
  });
}

/**
 * readConversations returns the persisted list, dropping anything it cannot
 * make sense of. A corrupt entry costs its own conversation, never the strip:
 * the dock is a helper, and it must not be possible to lock yourself out of it
 * by hand-editing localStorage or by a shape change between builds.
 */
export function readConversations(): Conversation[] {
  const raw = readStringFlag(conversationStorageKey(CONVERSATIONS_KEY), "");
  if (!raw) return [];
  try {
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    return dedupeRunIds(parsed.filter(isConversation).slice(0, MAX_CONVERSATIONS));
  } catch {
    return [];
  }
}

/**
 * dedupeRunIds enforces the invariant a run has exactly ONE conversation.
 *
 * Two conversations claiming the same run is not a hypothetical: before a
 * conversation owned its run, a remount re-ran the bot-scoped lookup and was
 * handed a neighbour's — and that stolen id then got RECORDED on both. Storage
 * therefore already holds the broken shape, and repairing it on read is what
 * makes the fix reach the operator instead of asking them to clear tabs by
 * hand.
 *
 * The first claimant keeps it. A later one is cleared rather than dropped: the
 * conversation is still the operator's, it simply has no run yet and will
 * launch its own at the next message.
 */
export function dedupeRunIds(list: readonly Conversation[]): Conversation[] {
  const claimed = new Set<string>();
  return list.map((c) => {
    if (!c.runId) return { ...c };
    if (claimed.has(c.runId)) {
      const {
        runId: _stolen,
        workspaceHandoffId: _stolenHandoff,
        lastReadWatchResultSeq: _stolenReadCursor,
        ...rest
      } = c;
      // `fresh` too: with no run of its own it must START EMPTY, not fall back
      // to the bot-scoped lookup — which is the very thing that would hand it
      // the neighbour's run again on the next mount.
      return { ...rest, fresh: true };
    }
    claimed.add(c.runId);
    return { ...c };
  });
}

/**
 * switchConversationBot retargets a tab onto another bot and drops the
 * run it owned. The previous bot's runId would otherwise be attached as
 * if it belonged to the new bot — transcript and steering both leak.
 */
export function switchConversationBot(
  list: readonly Conversation[],
  id: string,
  botId: string,
): Conversation[] {
  return list.map((c) => {
    if (c.id !== id) return c;
    const next: Conversation = { ...c, botId, fresh: true };
    delete next.runId;
    delete next.workspaceHandoffId;
    delete next.lastReadWatchResultSeq;
    return next;
  });
}

/**
 * claimRun records the run a conversation launched, refusing a run another
 * conversation already owns. The write-side half of the same invariant: the
 * session hook can still be handed a neighbour's run (a legacy conversation
 * with no id of its own falls back to the lookup), and recording that is what
 * turned a transient mix-up into a persisted one.
 */
export function claimRun(
  list: readonly Conversation[],
  id: string,
  runId: string,
): Conversation[] | null {
  const ownedElsewhere = list.some((c) => c.id !== id && c.runId === runId);
  if (ownedElsewhere) return null; // refused — the caller must not persist
  return list.map((c) => {
    if (c.id !== id) return { ...c };
    if (c.runId === runId) return { ...c, fresh: false };
    const next: Conversation = { ...c, fresh: false, runId };
    delete next.lastReadWatchResultSeq;
    return next;
  });
}

export function writeConversations(list: readonly Conversation[]): void {
  writeStringFlag(
    conversationStorageKey(CONVERSATIONS_KEY),
    JSON.stringify(list.slice(0, MAX_CONVERSATIONS)),
  );
}

export function readActiveConversation(): string {
  return readStringFlag(conversationStorageKey(ACTIVE_CONVERSATION_KEY), "");
}

export function writeActiveConversation(id: string): void {
  writeStringFlag(conversationStorageKey(ACTIVE_CONVERSATION_KEY), id);
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
