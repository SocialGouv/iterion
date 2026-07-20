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
      // Surface the bot-declared editor branding (first share wins — a team's
      // shares are typically all one bot) up to the shell header + heading.
      onBranding?.({ title: list[0]?.editor_title, description: list[0]?.editor_description });
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

  return (
    <div className="flex flex-col gap-4">
      <div>
        <h1 className="text-lg font-semibold text-fg-default">
          {shares?.[0]?.editor_title || "Config editor"}
        </h1>
        <p className="text-sm text-fg-muted">
          {shares?.[0]?.editor_description ? (
            <>
              {shares[0].editor_description}
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

function ShareList({
  shares,
  selectedId,
  onSelect,
}: {
  shares: EditorShare[];
  selectedId: string | null;
  onSelect: (id: string) => void;
}) {
  return (
    <ul className="flex flex-col gap-2" aria-label="Config-shares">
      {shares.map((s) => {
        const active = s.id === selectedId;
        return (
          <li key={s.id}>
            <button
              type="button"
              onClick={() => onSelect(s.id)}
              aria-current={active ? "true" : undefined}
              className={`w-full rounded-[var(--radius-lg)] border px-3 py-2 text-left transition-colors ${
                active
                  ? "border-accent bg-accent-soft/50"
                  : "border-border-default bg-surface-1 hover:border-border-strong"
              }`}
            >
              <div className="flex items-center justify-between gap-2">
                <span className="truncate text-sm font-medium text-fg-default">
                  {s.label || s.bot_id || s.id}
                </span>
                {s.read_only && (
                  <span className="shrink-0 text-caption text-fg-subtle">read-only</span>
                )}
              </div>
              <div className="mt-0.5 flex flex-wrap items-center gap-x-2 text-caption text-fg-subtle">
                {s.bot_id && <span className="truncate">{s.bot_id}</span>}
                {s.category && (
                  <>
                    <span aria-hidden>·</span>
                    <span className="truncate">{s.category}</span>
                  </>
                )}
              </div>
              {s.config_path && (
                <div className="mt-0.5 truncate font-mono text-caption text-fg-subtle">
                  {s.config_path}
                </div>
              )}
            </button>
          </li>
        );
      })}
    </ul>
  );
}
