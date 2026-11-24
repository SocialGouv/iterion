// The sources that launch the edited bot, on its start node (ADR-168 §10,
// #3275): one logo per connector its enabled subscriptions listen to, a
// label for every other family, a failing source marked as such, and — where
// the server plugs sources (cloud) — a link to plug another. A view: it
// reads the bot's subscriptions and writes nothing.
import { useQuery } from "@tanstack/react-query";
import { Link } from "wouter";

import { FeatureUnavailableError } from "@/api/client";
import { listTriggers } from "@/api/triggers";
import { useMaybeAuth } from "@/auth/AuthContext";
import { SourceLogo } from "@/components/Sources/SourceLogo";
import { useOpenBot } from "@/hooks/useBotForOpenFile";
import { sourcesOf } from "@/lib/startSources";
import { useServerInfoStore } from "@/store/serverInfo";

export function StartSourcesChip() {
  const bot = useOpenBot();
  // No auth context (a local studio, a canvas rendered alone) is no team.
  const activeTeamID = useMaybeAuth()?.activeTeamID ?? "";
  const cloud = useServerInfoStore((s) => s.info?.mode === "cloud");
  const botName = bot?.name ?? "";
  const subs = useQuery({
    // The team is part of the answer: a team switch reads its own.
    queryKey: ["triggers", "bot", activeTeamID, botName],
    queryFn: () => listTriggers({ bot: botName }),
    enabled: bot !== null,
  });
  // Nothing until the answer: an empty chip read before it would say "no
  // source" of a bot that has some.
  if (bot === null || subs.isPending) return null;
  // A server with no trigger store has no sources to show or plug.
  if (subs.error instanceof FeatureUnavailableError) return null;
  if (subs.error) {
    return (
      <div lang="fr" className="nodrag mt-1 text-caption text-warning-fg" title={String(subs.error)}>
        Sources illisibles
      </div>
    );
  }
  const sources = sourcesOf(subs.data ?? []);
  const named = sources.map((s) => (s.failing ? `${s.id} (en erreur)` : s.id));
  return (
    <div lang="fr" className="nodrag mt-1 flex items-center justify-center gap-1">
      {sources.length > 0 && (
        <span role="img" aria-label={`Sources : ${named.join(", ")}`} className="flex items-center gap-0.5">
          {sources.map((s) =>
            s.kind === "connector" ? (
              <SourceLogo
                key={s.id}
                connectorId={s.id}
                className={`h-4 w-4 rounded-[var(--radius-sm)]${s.failing ? " ring-2 ring-warning" : ""}`}
              />
            ) : (
              <span
                key={s.id}
                className={`rounded px-1 text-caption ${s.failing ? "bg-warning-soft text-warning-fg" : "bg-surface-2 text-fg-muted"}`}
              >
                {s.id}
              </span>
            ),
          )}
        </span>
      )}
      {cloud && (
        <Link href={`/sources/plug?bot=${encodeURIComponent(bot.name)}`} className="text-caption text-accent-text underline">
          {sources.length > 0 ? "+ source" : "Brancher une source"}
        </Link>
      )}
    </div>
  );
}
