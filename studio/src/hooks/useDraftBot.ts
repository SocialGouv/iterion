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

import { findDraftBotSource } from "@/api/runs/artifacts";

// A draft, once produced, does not change until the next turn — and the next
// turn moves the key. Long enough to stop re-fetching on every dock re-render
// (the session context changes on every websocket event).
const STALE_MS = 60 * 1000;

export function draftBotQueryKey(runId: string, revision: number) {
  return ["draft-bot", runId, revision] as const;
}

/**
 * useDraftBotAvailable reports whether `runId` published a `.bot` draft.
 *
 * `revision` should be something that advances once per turn (the transcript's
 * message count). A failed lookup reports "no draft" rather than surfacing an
 * error: the dock's offer is an affordance, and a missing one must never take
 * the conversation down with it.
 */
export function useDraftBotAvailable(
  runId: string | null,
  revision: number,
): boolean {
  const query = useQuery({
    queryKey: draftBotQueryKey(runId ?? "", revision),
    queryFn: ({ signal }) => findDraftBotSource(runId as string, { signal }),
    enabled: !!runId,
    staleTime: STALE_MS,
    retry: false,
  });
  return typeof query.data === "string" && query.data.trim() !== "";
}
