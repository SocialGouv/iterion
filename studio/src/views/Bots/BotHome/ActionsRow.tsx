// Extracted from BotHome/index.tsx to keep that file focused.
// Actions row — Launch / Open in editor / Pipeline board / Test, plus
// the cloud repo-context line (bound repositories + guided bind flow).

import { useMemo } from "react";
import { Link, useLocation } from "wouter";

import type { BotEntryWithSchema } from "@/api/bots";
import { forgeTeamRepoKey } from "@/api/forgeConnections";
import { Button } from "@/components/ui";
import { useActiveRepo } from "@/hooks/useActiveRepo";
import { useServerInfoStore } from "@/store/serverInfo";
import { useTabsStore } from "@/store/tabs";
import { bindBotPath } from "@/views/integrations/wizard/bindModel";
import { repoDetailPath } from "@/views/RepoDetail/repoKey";

export default function ActionsRow({
  entry,
  launchFile,
  testOpen,
  onToggleTest,
}: {
  entry: BotEntryWithSchema;
  launchFile: string | null;
  testOpen: boolean;
  onToggleTest: () => void;
}) {
  const [, setLocation] = useLocation();
  const pipelineBoardsEnabled = useServerInfoStore(
    (state) => !!state.info?.native_tracker_enabled,
  );
  const isCloud = useServerInfoStore((s) => s.info?.mode === "cloud");
  const { activeRepo, repos, enabled: repoScopeEnabled } = useActiveRepo();

  const noPathTitle =
    "The server couldn't relativise this bot's path to the workspace — launch it from its own directory instead.";

  const onLaunch = () => {
    if (!launchFile) return;
    // Prefer the clean, shareable slug URL (/runs/new?bot=feed-watch) over the
    // encoded file path; LaunchView resolves the slug back to launchFile. The
    // entry name is the catalog slug and is always set.
    setLocation(`/runs/new?bot=${encodeURIComponent(entry.name)}`);
  };

  const onEdit = () => {
    if (!launchFile) return;
    useTabsStore.getState().openTab("editor", { file: launchFile });
    setLocation(`/editor?file=${encodeURIComponent(launchFile)}`);
  };

  // Cloud repo-context row: the repositories this bot is bound to (webhook
  // provisioned), plus the guided bind flow. A manifest `repo: required`
  // with nothing bound surfaces as a warning instead of a passive line.
  const repoReq = entry.repo && entry.repo.mode !== "none" ? entry.repo : null;
  const boundRepos = useMemo(
    () => repos.filter((r) => r.bot_ids.includes(entry.name)),
    [repos, entry.name],
  );
  const showRepoContext =
    isCloud && repoScopeEnabled && (!!repoReq || boundRepos.length > 0);
  const needsRepo =
    repoReq?.mode === "required" && boundRepos.length === 0 && !activeRepo;
  const bindHref = bindBotPath({
    bot: entry.name,
    returnTo: `/bots/${encodeURIComponent(entry.name)}`,
  });

  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex flex-wrap items-center gap-2">
        <Button
          variant="primary"
          size="sm"
          onClick={onLaunch}
          disabled={!launchFile}
          title={launchFile ? `Configure and launch ${entry.name}` : noPathTitle}
        >
          Launch
        </Button>
        {/* Cloud has no editor save path (the Editor is out of the cloud
            nav), so the button is hidden rather than disabled — it has no
            cloud equivalent. */}
        {!isCloud && (
          <Button
            variant="secondary"
            size="sm"
            onClick={onEdit}
            disabled={!launchFile}
            title={launchFile ? "Open the workflow in the editor" : noPathTitle}
          >
            Open in editor
          </Button>
        )}
        {pipelineBoardsEnabled && (
          <Button
            variant="secondary"
            size="sm"
            onClick={() => setLocation("/pipelines")}
            title="Open the global pipeline board"
          >
            Pipeline board
          </Button>
        )}
        {/* Test opens the embedded TestRunPane (contained run: commits land
            on a storage branch only) instead of navigating away. */}
        <Button
          variant={testOpen ? "primary" : "secondary"}
          size="sm"
          onClick={onToggleTest}
          disabled={!launchFile}
          aria-expanded={testOpen}
          title={
            launchFile
              ? testOpen
                ? "Close the test pane"
                : "Open an embedded test pane (contained run — commits stay on a storage branch, no merge)"
              : noPathTitle
          }
        >
          {testOpen ? "Close test" : "Test"}
        </Button>
      </div>
      {showRepoContext && (
        <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
          {boundRepos.length > 0 ? (
            <span className="flex flex-wrap items-center gap-x-2 gap-y-1 text-caption text-fg-subtle">
              Bound to
              {boundRepos.map((r) => (
                <Link
                  key={forgeTeamRepoKey(r)}
                  href={repoDetailPath(r)}
                  className="font-mono text-accent-text hover:underline"
                  title={`Repository details — ${r.repo_full_name}`}
                >
                  {r.repo_full_name}
                </Link>
              ))}
            </span>
          ) : (
            <span
              className={`text-caption ${needsRepo ? "text-warning-fg" : "text-fg-subtle"}`}
            >
              {needsRepo
                ? "Needs a target repository."
                : "Not bound to any repository yet."}
            </span>
          )}
          <Link href={bindHref}>
            <Button variant="secondary" size="sm">
              Bind to repository…
            </Button>
          </Link>
        </div>
      )}
    </div>
  );
}
