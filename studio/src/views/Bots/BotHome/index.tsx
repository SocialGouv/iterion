import { useEffect, useMemo, useState } from "react";
import { Link, useParams } from "wouter";

import type { BotEntryWithSchema } from "@/api/bots";
import TestRunPane from "@/components/Runs/TestRunPane";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import { Button, EmptyState, InlineBanner, Spinner } from "@/components/ui";
import { useBotsStore } from "@/store/bots";
import { useServerInfoStore } from "@/store/serverInfo";

import { botLaunchFile } from "../botPaths";
import ActionsRow from "./ActionsRow";
import AutomationsCard from "./AutomationsCard";
import { ConfigSharesCard } from "./ConfigSharesCard";
import IdentityHeader from "./IdentityHeader";
import MetadataCard from "./MetadataCard";
import PresetsCard from "./PresetsCard";
import RecentRunsCard from "./RecentRunsCard";
import SecretBindingsCard from "./SecretBindingsCard";
import VarsCard from "./VarsCard";

/**
 * BotHomeView — /bots/:name — one bot's home page: identity + activation,
 * launch/edit/test actions, manifest metadata, automations (suggested
 * manifest invocations + active trigger subscriptions), vars, presets,
 * recent runs, and (cloud) secret bindings. Each section lives in its
 * own sibling component; this file resolves the bot entry and lays the
 * cards out.
 */
export default function BotHomeView() {
  const params = useParams<{ name: string }>();
  const name = decodeURIComponent(params.name ?? "");
  const bots = useBotsStore((s) => s.bots);
  const loading = useBotsStore((s) => s.loading);
  const botsError = useBotsStore((s) => s.error);
  const fetchBots = useBotsStore((s) => s.fetch);

  useEffect(() => {
    if (bots === null) void fetchBots();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const entry = useMemo(() => (bots ?? []).find((b) => b.name === name), [bots, name]);
  const label = entry?.display_name?.trim() || name;

  useHeaderSlot({
    left: (
      <span className="flex items-center gap-1.5 text-xs font-medium text-fg-default">
        <Link href="/bots" className="text-fg-muted hover:text-fg-default hover:underline">
          Bots
        </Link>
        <span className="text-fg-subtle">/</span>
        <span>{label}</span>
      </span>
    ),
  });

  if (botsError) {
    return (
      <div className="p-4">
        <InlineBanner tone="danger" title="Couldn't load bots">
          {botsError}
        </InlineBanner>
      </div>
    );
  }
  if (!entry) {
    if (bots === null || loading) {
      return (
        <div className="flex items-center gap-2 p-6 text-sm text-fg-muted">
          <Spinner /> Loading bot…
        </div>
      );
    }
    return (
      <div className="p-6">
        <EmptyState
          title="Bot not found"
          message={`No bot named “${name}” is discovered in this workspace.`}
          action={
            <Link href="/bots">
              <Button variant="secondary" size="sm">
                Back to Bots
              </Button>
            </Link>
          }
        />
      </div>
    );
  }
  return <BotHome entry={entry} />;
}

function BotHome({ entry }: { entry: BotEntryWithSchema }) {
  const serverInfo = useServerInfoStore((s) => s.info);
  const launchFile = botLaunchFile(entry);
  const [testOpen, setTestOpen] = useState(false);

  const main = (
    <>
      <IdentityHeader entry={entry} />
      <ActionsRow
        entry={entry}
        launchFile={launchFile}
        testOpen={testOpen}
        onToggleTest={() => setTestOpen((v) => !v)}
      />
      {entry.schema_error && (
        <InlineBanner tone="warning" title="Workflow failed to parse">
          {entry.schema_error}
        </InlineBanner>
      )}
      <MetadataCard entry={entry} />
      <AutomationsCard entry={entry} />
      <VarsCard entry={entry} />
      {entry.presets && (entry.presets.entries?.length ?? 0) > 0 && (
        <PresetsCard entry={entry} />
      )}
      <RecentRunsCard botName={entry.name} />
      {serverInfo?.mode === "cloud" && <SecretBindingsCard botName={entry.name} />}
      {serverInfo?.config_shares_enabled && <ConfigSharesCard entry={entry} />}
    </>
  );

  // <main> is overflow-hidden (each view owns its scroll, like Secrets/Skills):
  // a full-height overflow-y-auto wrapper keeps a tall bot home (metadata +
  // automations + vars + config-shares) reachable, scrollbar at the edge.
  if (!testOpen || !launchFile) {
    return (
      <div className="h-full overflow-y-auto">
        <div className="mx-auto flex w-full max-w-3xl flex-col gap-3 p-4">{main}</div>
      </div>
    );
  }

  // Test pane open: on xl the pane docks as a sticky right column next
  // to the existing sections; below xl it stacks as a bottom section.
  return (
    <div className="h-full overflow-y-auto">
      <div className="mx-auto flex w-full max-w-7xl flex-col gap-4 p-4 xl:flex-row xl:items-start">
        <div className="mx-auto flex w-full max-w-3xl flex-col gap-3 xl:mx-0 xl:min-w-0 xl:flex-1">
          {main}
        </div>
        <div className="w-full xl:sticky xl:top-4 xl:w-[460px] xl:shrink-0">
          <TestRunPane
            file={launchFile}
            vars={entry.vars?.fields ?? []}
            onClose={() => setTestOpen(false)}
          />
        </div>
      </div>
    </div>
  );
}
