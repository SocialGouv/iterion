import { useEffect, useMemo } from "react";

import type { BotEntryWithSchema } from "@/api/bots";
import { useMaybeAuth } from "@/auth/AuthContext";
import { resolveOpenBot } from "@/lib/startSources";
import { useBotsStore } from "@/store/bots";
import { useDocumentStore } from "@/store/document";

/**
 * useOpenBot resolves the registry bot the editor's open file is — a
 * bundle's main.bot, a loose bot, or a team's bot opened from its source
 * (resolveOpenBot, the one reading of "which bot is open") — or null. It
 * triggers a one-time bots fetch so it answers on a cold editor load.
 */
export function useOpenBot(): BotEntryWithSchema | null {
  const currentFilePath = useDocumentStore((s) => s.currentFilePath);
  const bots = useBotsStore((s) => s.bots);
  const fetchBots = useBotsStore((s) => s.fetch);
  // No auth context (a local studio, a canvas rendered alone) is no team.
  const activeTeamID = useMaybeAuth()?.activeTeamID ?? "";

  useEffect(() => {
    if (bots === null) void fetchBots();
  }, [bots, fetchBots]);

  return useMemo(
    () => resolveOpenBot(bots, currentFilePath, activeTeamID) ?? null,
    [bots, currentFilePath, activeTeamID],
  );
}

/**
 * useBotForOpenFile is useOpenBot narrowed to a bundle: the conditional
 * Inspector "Bot" metadata tab edits a bundle's manifest, which a loose
 * .bot has none of.
 */
export function useBotForOpenFile(): BotEntryWithSchema | null {
  const bot = useOpenBot();
  return bot?.is_bundle ? bot : null;
}
