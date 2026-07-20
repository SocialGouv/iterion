import { useCallback, useEffect, useState } from "react";

import { errorMessage } from "@/lib/errorHints";
import { listEditorShares, type EditorShare } from "@/api/configEditor";
import { Button, Card, EmptyState, InlineBanner, Spinner } from "@/components/ui";

import { ShareEditor } from "./ShareEditor";

// ---------------------------------------------------------------------------
// Workspace — share list (master) + editor (detail).
// ---------------------------------------------------------------------------

export function Workspace({
  teamID,
  teamName,
  onBranding,
}: {
  teamID: string;
  teamName?: string;
  onBranding?: (b: { title?: string; description?: string }) => void;
}) {
  const [shares, setShares] = useState<EditorShare[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [selectedId, setSelectedId] = useState<string | null>(null);

  const load = useCallback(async () => {
    setError(null);
    try {
      const list = await listEditorShares(teamID);
      setShares(list);
      // Auto-select the first share so the editor isn't an empty right pane.
      setSelectedId((cur) => cur ?? list[0]?.id ?? null);
      // Surface the bot-declared editor branding to the shell header — only
      // when all shares are one bot; a multi-bot team gets the generic shell
      // title (the per-bot branding then lives in the group headers).
      const oneBot = new Set(list.map((s) => s.bot_id)).size <= 1;
      onBranding?.({
        title: oneBot ? list[0]?.editor_title : undefined,
        description: oneBot ? list[0]?.editor_description : undefined,
      });
    } catch (err) {
      setShares([]);
      setError(errorMessage(err));
    }
  }, [teamID, onBranding]);

  useEffect(() => {
    void load();
  }, [load]);

  if (shares === null && !error) {
    return (
      <div className="flex items-center gap-2 p-6 text-sm text-fg-muted">
        <Spinner /> Loading config-shares…
      </div>
    );
  }

  const selected = shares?.find((s) => s.id === selectedId) ?? null;

  // A single-bot editor keeps that bot's branding as the heading; once shares
  // span several bots the branding moves to the per-bot group header and the
  // page heading is generic.
  const singleBot = shares ? new Set(shares.map((s) => s.bot_id)).size <= 1 : false;
  const brandTitle = singleBot ? shares?.[0]?.editor_title : undefined;
  const brandDescription = singleBot ? shares?.[0]?.editor_description : undefined;

  return (
    <div className="flex flex-col gap-4">
      <div>
        <h1 className="text-lg font-semibold text-fg-default">
          {brandTitle || "Config editor"}
        </h1>
        <p className="text-sm text-fg-muted">
          {brandDescription ? (
            <>
              {brandDescription}
              {teamName ? (
                <>
                  {" "}
                  <span className="text-fg-subtle">
                    · <span className="font-medium text-fg-default">{teamName}</span>
                  </span>
                </>
              ) : null}
            </>
          ) : (
            <>
              Edit the config-shares
              {teamName ? (
                <>
                  {" "}for <span className="font-medium text-fg-default">{teamName}</span>
                </>
              ) : null}
              . Only the fields listed for each share are editable.
            </>
          )}
        </p>
      </div>

      {error && (
        <InlineBanner tone="danger" layout="inline" title="Couldn't load config-shares">
          {error}
          <div className="mt-2">
            <Button variant="secondary" size="sm" onClick={() => void load()}>
              Try again
            </Button>
          </div>
        </InlineBanner>
      )}

      {shares && shares.length === 0 && !error ? (
        <Card>
          <EmptyState
            title="No config-shares"
            message="This team has no config-shares assigned to you yet. Ask an administrator to create one."
          />
        </Card>
      ) : shares && shares.length > 0 ? (
        <div className="grid grid-cols-1 gap-4 md:grid-cols-[minmax(220px,300px)_1fr]">
          <ShareList
            shares={shares}
            selectedId={selectedId}
            onSelect={(id) => setSelectedId(id)}
          />
          <div className="min-w-0">
            {selected ? (
              <ShareEditor key={selected.id} teamID={teamID} share={selected} />
            ) : (
              <Card>
                <EmptyState message="Select a config-share on the left to edit it." />
              </Card>
            )}
          </div>
        </div>
      ) : null}
    </div>
  );
}

// shortRepo reduces a repo URL to "org/repo" for a compact group header.
function shortRepo(url: string): string {
  if (!url) return "Other";
  const cleaned = url.replace(/\.git$/, "").replace(/\/+$/, "");
  const m = cleaned.match(/([^/]+\/[^/]+)$/);
  return m?.[1] ?? cleaned;
}

interface BotGroup {
  botId: string;
  botLabel: string;
  shares: EditorShare[];
}
interface RepoGroup {
  repoKey: string;
  repoLabel: string;
  bots: BotGroup[];
}

// groupShares builds the repo → bot → shares hierarchy the editor renders, so a
// team whose config-shares span several bots and repos reads as a tree instead
// of one flat list. Insertion order is preserved (the server already sorts).
function groupShares(shares: EditorShare[]): RepoGroup[] {
  const repos = new Map<string, Map<string, EditorShare[]>>();
  for (const s of shares) {
    const rk = s.repo_url ?? "";
    const bk = s.bot_id ?? "";
    let bots = repos.get(rk);
    if (!bots) {
      bots = new Map();
      repos.set(rk, bots);
    }
    const bucket = bots.get(bk);
    if (bucket) bucket.push(s);
    else bots.set(bk, [s]);
  }
  return Array.from(repos, ([rk, bots]) => ({
    repoKey: rk,
    repoLabel: shortRepo(rk),
    bots: Array.from(bots, ([bk, ss]) => ({
      botId: bk,
      // The bot's own branding (manifest editor_title) names the group; fall
      // back to the technical bot id.
      botLabel: ss[0]?.editor_title || bk || "Config",
      shares: ss,
    })),
  }));
}

function ShareList({
  shares,
  selectedId,
  onSelect,
}: {
  shares: EditorShare[];
  selectedId: string | null;
  onSelect: (id: string) => void;
}) {
  const groups = groupShares(shares);
  return (
    <div className="flex flex-col gap-4" aria-label="Config-shares">
      {groups.map((repo) => (
        <div key={repo.repoKey || "other"} className="flex flex-col gap-2">
          <div
            className="truncate font-mono text-caption font-semibold uppercase tracking-wider text-fg-subtle"
            title={repo.repoKey}
          >
            {repo.repoLabel}
          </div>
          {repo.bots.map((bot) => (
            <div key={bot.botId} className="flex flex-col gap-1.5 border-l border-border-subtle pl-2.5">
              <div className="flex items-baseline gap-2">
                <span className="truncate text-sm font-medium text-fg-default">
                  {bot.botLabel}
                </span>
                {bot.botLabel !== bot.botId && bot.botId && (
                  <span className="truncate font-mono text-caption text-fg-subtle">
                    {bot.botId}
                  </span>
                )}
              </div>
              <ul className="flex flex-col gap-1.5">
                {bot.shares.map((s) => (
                  <li key={s.id}>
                    <ShareButton
                      share={s}
                      active={s.id === selectedId}
                      onSelect={() => onSelect(s.id)}
                    />
                  </li>
                ))}
              </ul>
            </div>
          ))}
        </div>
      ))}
    </div>
  );
}

function ShareButton({
  share: s,
  active,
  onSelect,
}: {
  share: EditorShare;
  active: boolean;
  onSelect: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onSelect}
      aria-current={active ? "true" : undefined}
      className={`w-full rounded-lg border px-3 py-2 text-left transition-colors ${
        active
          ? "border-accent bg-accent-soft/50"
          : "border-border-default bg-surface-1 hover:border-border-strong"
      }`}
    >
      <div className="flex items-center justify-between gap-2">
        <span className="truncate text-sm font-medium text-fg-default">
          {s.label || s.category || s.id}
        </span>
        {s.read_only && (
          <span className="shrink-0 text-caption text-fg-subtle">read-only</span>
        )}
      </div>
      <div className="mt-0.5 flex flex-wrap items-center gap-x-2 text-caption text-fg-subtle">
        {s.category && <span className="truncate">{s.category}</span>}
        {s.config_path && (
          <>
            <span aria-hidden>·</span>
            <span className="truncate font-mono">{s.config_path}</span>
          </>
        )}
      </div>
    </button>
  );
}
