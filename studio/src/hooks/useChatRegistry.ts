// The assistant's bot registry, discovered from the server's bot listing.
//
// The const it replaces named one bot; this asks the server which bots
// declare a `chat:` block. Adding a second conversational bot is therefore a
// manifest edit — the acceptance criterion of issue #333 and the studio-side
// half of CLAUDE.md's "the engine stays bot-agnostic".

import { useCallback, useMemo } from "react";
import { useQuery } from "@tanstack/react-query";

import { listBots, type BotEntry } from "@/api/bots";
import { errorMessage } from "@/lib/errorHints";
import {
  FIRST_CLASS_BOTS,
  DEFAULT_WHATS_NEXT_BOT_ID,
  type FirstClassBot,
} from "@/lib/whats-next/firstClassBots";
import { chatRegistryFrom } from "@/lib/whats-next/chatRegistry";

// The catalog is filesystem state that changes when someone edits a manifest,
// not live data. The provider lives above the route tree and never remounts,
// so poll at the same five-minute cadence: a manifest edit/disable must not
// leave a stale bot in the dock until the whole SPA is reloaded.
const STALE_MS = 5 * 60 * 1000;

export const CHAT_REGISTRY_QUERY_KEY = ["chat-registry"] as const;

// The dock's preferred correspondent. Nexie owns /whats-next and ONLY that
// route (see resolveDockBot); the ubiquitous dock is the general iterion
// assistant. Preferred, not required: a workspace without this bundle falls
// back to whatever other chat bot it discovered, so the dock still works.
export const PREFERRED_DOCK_BOT_ID = "copilot";

export interface UseChatRegistryResult {
  /** Every conversational bot the server offers, id → entry. */
  byId: Record<string, FirstClassBot>;
  /** Stable display order: the built-in default first (it is the one an
   *  operator has muscle memory for), then alphabetical by label. */
  bots: FirstClassBot[];
  /** The bots the DOCK may host — every conversational bot except the one
   *  that owns /whats-next. */
  dockBots: FirstClassBot[];
  /** Resolve one bot, falling back to the built-in entry so the dock keeps
   *  working while the listing is in flight or the endpoint is unavailable. */
  resolve: (id: string | null | undefined) => FirstClassBot | null;
  /** Resolve the DOCK's correspondent: never the /whats-next bot, and
   *  defaulting to the iterion assistant rather than to Nexie. */
  resolveDock: (id: string | null | undefined) => FirstClassBot | null;
  loading: boolean;
  error: string | null;
}

// Compose the discovered registry over the built-in floor.
//
// The built-in entry is a FLOOR, not an override: a discovered manifest wins
// for the same id. Without the floor, a studio whose /api/v1/bots is
// unreachable (or a workspace with no bots/ directory — the desktop app
// pointed at a bare repo) would show no assistant at all, which reads as
// "the product lost a feature" rather than "discovery is empty".
//
// But a listing that REPORTS a bot as disabled is discovery ANSWERING "no",
// not failing to answer. /api/v1/bots deliberately includes disabled entries
// (pkg/server/bots_routes.go: "included (Enabled=false) so the studio can
// show them to flip back on"), and chatRegistryFrom drops them — "one
// visibility decision, not two". Flooring such an entry back would make the
// Catalog manager's toggle silently inert for the one surface that reads the
// registry as authoritative, and for the default id it would keep that bot
// selected ahead of the bots the operator did enable. So the floor covers
// "discovery could not answer", never "discovery answered no".
export function chatRegistryWithFloor(
  entries: readonly BotEntry[],
): Record<string, FirstClassBot> {
  const discovered = chatRegistryFrom(entries);
  const disabled = new Set(
    entries.filter((e) => e.enabled === false).map((e) => e.name),
  );
  const floor = Object.fromEntries(
    Object.entries(FIRST_CLASS_BOTS).filter(([id]) => !disabled.has(id)),
  );
  return { ...floor, ...discovered };
}

export function resolveChatBot(
  byId: Record<string, FirstClassBot>,
  bots: FirstClassBot[],
  id: string | null | undefined,
  loading: boolean,
): FirstClassBot | null {
  if (id && byId[id]) return byId[id];
  // A persisted non-default id is not "unknown" until discovery settles.
  // Park the session on its empty fallback so startup discovery never probes,
  // attaches or accepts input for the default bot during a cold load.
  if (id && loading) return null;
  return byId[DEFAULT_WHATS_NEXT_BOT_ID] ?? bots[0] ?? null;
}

// The dock's own resolution. It differs from resolveChatBot in what it
// REFUSES: the /whats-next bot never answers in the dock, so a persisted
// selection naming it (or a stale one from before the split) resolves to the
// dock's default instead of resurrecting Nexie on /board.
export function resolveDockBot(
  byId: Record<string, FirstClassBot>,
  dockBots: FirstClassBot[],
  id: string | null | undefined,
  loading: boolean,
): FirstClassBot | null {
  if (id && id !== DEFAULT_WHATS_NEXT_BOT_ID && byId[id]) return byId[id];
  // Same cold-load rule as resolveChatBot: a persisted id is not "unknown"
  // until discovery settles, so park rather than probe the default.
  if (id && id !== DEFAULT_WHATS_NEXT_BOT_ID && loading) return null;
  return byId[PREFERRED_DOCK_BOT_ID] ?? dockBots[0] ?? null;
}

export function useChatRegistry(): UseChatRegistryResult {
  const query = useQuery({
    queryKey: CHAT_REGISTRY_QUERY_KEY,
    queryFn: listBots,
    staleTime: STALE_MS,
    // A failed listing must not blank the assistant: the built-in fallback
    // below covers it, and retrying forever on a server that does not expose
    // the endpoint just burns requests.
    retry: 1,
    refetchInterval: STALE_MS,
  });

  const byId = useMemo(() => chatRegistryWithFloor(query.data ?? []), [query.data]);

  const bots = useMemo(() => {
    const all = Object.values(byId);
    return all.sort((a, b) => {
      if (a.id === DEFAULT_WHATS_NEXT_BOT_ID) return -1;
      if (b.id === DEFAULT_WHATS_NEXT_BOT_ID) return 1;
      return a.label.localeCompare(b.label);
    });
  }, [byId]);

  const dockBots = useMemo(
    () => bots.filter((b) => b.id !== DEFAULT_WHATS_NEXT_BOT_ID),
    [bots],
  );

  const resolve = useCallback(
    (id: string | null | undefined): FirstClassBot | null =>
      resolveChatBot(byId, bots, id, query.isLoading),
    [byId, bots, query.isLoading],
  );

  const resolveDock = useCallback(
    (id: string | null | undefined): FirstClassBot | null =>
      resolveDockBot(byId, dockBots, id, query.isLoading),
    [byId, dockBots, query.isLoading],
  );

  return {
    byId,
    bots,
    dockBots,
    resolve,
    resolveDock,
    loading: query.isLoading,
    error: query.error ? errorMessage(query.error) : null,
  };
}
