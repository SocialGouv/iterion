import { useCallback, useEffect, useState } from "react";

import { errorMessage } from "@/lib/errorHints";
import { listEditorShares, type EditorShare } from "@/api/configEditor";
import { Button, Card, EmptyState, InlineBanner, Spinner } from "@/components/ui";

import { ShareEditor } from "./ShareEditor";
import { ShareBrowser } from "./ShareBrowser";

// ---------------------------------------------------------------------------
// Workspace — share list (master) + editor (detail).
// ---------------------------------------------------------------------------

export function Workspace({
  teamID,
  teamName,
  onBranding,
  initialQuery,
}: {
  teamID: string;
  teamName?: string;
  onBranding?: (b: { title?: string; description?: string }) => void;
  // Forwarded to the browser as its initial filter (e.g. a bot slug when the
  // user arrived from that bot's page).
  initialQuery?: string;
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
        <div className="grid grid-cols-1 gap-4 md:grid-cols-[minmax(260px,340px)_1fr]">
          <ShareBrowser
            shares={shares}
            selectedId={selectedId}
            onSelect={(id) => setSelectedId(id)}
            initialQuery={initialQuery}
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
