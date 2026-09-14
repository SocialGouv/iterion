// Does the conversation currently on screen carry a `.bot` draft the operator
// could open in the editor?
//
// The assistant cannot write to the workspace — that read-only property is the
// point of the copilot — so a draft it produced exists ONLY as a node artifact
// on its own run. This hook is what turns that artifact into an offer to
// navigate: the dock proposes a link, the operator clicks, the editor loads it.
// Nothing moves without that click.
//
// Keyed on the message count rather than polled: a draft can only appear as the
// result of a turn, and a turn always lands messages in the transcript.

import { useQuery } from "@tanstack/react-query";

import { lookupDraft, type DraftLookup } from "@/api/runs/artifacts";

// A draft, once produced, does not change until the next turn — and the next
// turn moves the key. Long enough to stop re-fetching on every dock re-render
// (the session context changes on every websocket event).
const STALE_MS = 60 * 1000;

export function draftBotQueryKey(runId: string, revision: number) {
  return ["draft-bot", runId, revision] as const;
}

// The key an open editor tab watches. Deliberately WITHOUT a revision: the tab
// has no idea how many turns the conversation has had, and does not need to —
// it is told when to look again.
export function editorDraftKey(runId: string) {
  return ["editor-draft", runId] as const;
}

/**
 * useDraftState reports what the conversation has to offer the editor.
 *
 * TWO states, because the operator asked for the work to happen in the right
 * place BEFORE it happens: a turn can be DESIGNING with nothing to show yet
 * (offer the editor), or carry a finished draft (offer the draft). Collapsing
 * them would put the invitation after the work again.
 *
 * `revision` should advance once per turn (the transcript's message count). A
 * failed lookup reports "nothing" rather than surfacing an error: the offer is
 * an affordance, and a missing one must never take the conversation down.
 */
export function useDraftState(
  runId: string | null,
  revision: number,
): DraftLookup {
  const query = useQuery({
    queryKey: draftBotQueryKey(runId ?? "", revision),
    queryFn: ({ signal }) => lookupDraft(runId as string, { signal }),
    enabled: !!runId,
    staleTime: STALE_MS,
    retry: false,
  });
  return query.data ?? { source: null, designing: false };
}
