// The assistant's bot registry, discovered from the server's bot listing.
//
// The const it replaces named one bot; this asks the server which bots
// declare a `chat:` block. Adding a second conversational bot is therefore a
// manifest edit — the acceptance criterion of issue #333 and the studio-side
// half of CLAUDE.md's "the engine stays bot-agnostic".

import { useCallback, useMemo } from "react";
import { useQuery } from "@tanstack/react-query";

import { listBots } from "@/api/bots";
import { errorMessage } from "@/lib/errorHints";
import {
  FIRST_CLASS_BOTS,
  DEFAULT_WHATS_NEXT_BOT_ID,
  type FirstClassBot,
} from "@/lib/whats-next/firstClassBots";
import { chatRegistryFrom } from "@/lib/whats-next/chatRegistry";

// The catalog is filesystem state that changes when someone edits a manifest,
// not live data. Five minutes matches useModelCatalog; the Catalog manager
// invalidates on save through the same query key.
const STALE_MS = 5 * 60 * 1000;

export const CHAT_REGISTRY_QUERY_KEY = ["chat-registry"] as const;

export interface UseChatRegistryResult {
  /** Every conversational bot the server offers, id → entry. */
  byId: Record<string, FirstClassBot>;
  /** Stable display order: the built-in default first (it is the one an
   *  operator has muscle memory for), then alphabetical by label. */
  bots: FirstClassBot[];
  /** Resolve one bot, falling back to the built-in entry so the dock keeps
   *  working while the listing is in flight or the endpoint is unavailable. */
  resolve: (id: string | null | undefined) => FirstClassBot | null;
  loading: boolean;
  error: string | null;
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

export function useChatRegistry(): UseChatRegistryResult {
  const query = useQuery({
    queryKey: CHAT_REGISTRY_QUERY_KEY,
    queryFn: listBots,
    staleTime: STALE_MS,
    // A failed listing must not blank the assistant: the built-in fallback
    // below covers it, and retrying forever on a server that does not expose
    // the endpoint just burns requests.
    retry: 1,
  });

  const byId = useMemo(() => {
    const discovered = chatRegistryFrom(query.data ?? []);
    // The built-in entry is a FLOOR, not an override: a discovered manifest
    // wins for the same id. Without the floor, a studio whose /api/v1/bots is
    // unreachable (or a workspace with no bots/ directory — the desktop app
    // pointed at a bare repo) would show no assistant at all, which reads as
    // "the product lost a feature" rather than "discovery is empty".
    return { ...FIRST_CLASS_BOTS, ...discovered };
  }, [query.data]);

  const bots = useMemo(() => {
    const all = Object.values(byId);
    return all.sort((a, b) => {
      if (a.id === DEFAULT_WHATS_NEXT_BOT_ID) return -1;
      if (b.id === DEFAULT_WHATS_NEXT_BOT_ID) return 1;
      return a.label.localeCompare(b.label);
    });
  }, [byId]);

  const resolve = useCallback(
    (id: string | null | undefined): FirstClassBot | null =>
      resolveChatBot(byId, bots, id, query.isLoading),
    [byId, bots, query.isLoading],
  );

  return {
    byId,
    bots,
    resolve,
    loading: query.isLoading,
    error: query.error ? errorMessage(query.error) : null,
  };
}
