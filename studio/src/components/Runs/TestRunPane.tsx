// TestRunPane — a self-contained, embeddable "create → test → iterate"
// pane (the Mistral-style side-by-side loop). Mounted by BotHome (the
// Test action) and by the BotBuilder right column after a successful
// create. It launches a CONTAINED run (merge_into: "none" — commits
// stay on a storage branch) and then streams the live transcript
// inline: RunConversationView + AgentChatboxInline over the per-run
// store, so no full RunView tab is needed.

import { useEffect, useMemo, useRef, useState } from "react";
import { Cross1Icon } from "@radix-ui/react-icons";
import { Link, useLocation } from "wouter";

import { createRun } from "@/api/runs";
import type { RunStatus } from "@/api/runs";
import type { VarField } from "@/api/types";
import { useMaybeAuth } from "@/auth/AuthContext";
import RunConversationView from "@/components/Runs/conversation/RunConversationView";
import { buildVarsPayload } from "@/components/Runs/launchView/varsPayload";
import { STATUS_VARIANT, labelForStatus } from "@/components/Runs/runStatusMeta";
import { useRunSnapshot } from "@/components/Runs/runView/useRunSnapshot";
import AgentChatboxInline from "@/components/shared/AgentChatboxInline";
import { defaultStringFor } from "@/components/shared/VarFieldInput";
import {
  Badge,
  Button,
  IconButton,
  InlineBanner,
  Input,
  Spinner,
} from "@/components/ui";
import { useRunWebSocket } from "@/hooks/useRunWebSocket";
import { errorMessage } from "@/lib/errorHints";
import {
  botLaunchKey,
  fileLaunchKey,
  sendLaunch,
  useLaunchInFlight,
  type LaunchTarget,
} from "@/lib/sendLaunch";
import { RunStoreProvider, getOrCreateRunStore, useRunStore } from "@/store/run";
import { useServerInfoStore } from "@/store/serverInfo";
import type { BotLaunch } from "@/views/Bots/botPaths";

export interface TestRunPaneProps {
  /** How the bot under test is launched (botLaunch): by its slug in cloud —
   *  the server resolves the tier and the run carries the bot's identity —
   *  or by its workspace-relative .bot path locally. */
  launch: BotLaunch;
  /** The bot's declared vars — rendered as quick inputs pre-filled with
   *  their defaults so a required-var bot can still be test-launched. */
  vars?: VarField[];
  onClose?: () => void;
}

const TERMINAL_STATUSES: ReadonlySet<RunStatus> = new Set<RunStatus>([
  "finished",
  "failed",
  "failed_resumable",
  "cancelled",
]);

// The pane of ONE target. Everything it holds — the run it shows, the values
// typed, whether it is still shown at an answer — is that target's, so it is
// keyed by what it launches (the launch key, team included): rendered again
// for another bot or under another team, by a parent that kept it mounted, it
// starts from nothing instead of showing one bot's run or posting one bot's
// values under another.
export default function TestRunPane(props: TestRunPaneProps) {
  const teamScope = useMaybeAuth()?.activeTeamID ?? "";
  const workDir = useServerInfoStore((s) => s.info?.work_dir);
  const { launch } = props;
  const target: LaunchTarget =
    launch.by === "bot"
      ? {
          key: botLaunchKey(teamScope, launch.bot),
          label: `the bot ${launch.bot}`,
          runsHref: `/runs?bot=${encodeURIComponent(launch.bot)}`,
        }
      : {
          key: fileLaunchKey(teamScope, launch.file, workDir),
          label: launch.file,
          runsHref: "/runs",
        };
  return <TargetPane key={target.key} target={target} {...props} />;
}

function TargetPane({
  launch,
  vars = [],
  onClose,
  target,
}: TestRunPaneProps & { target: LaunchTarget }) {
  const [runId, setRunId] = useState<string | null>(null);

  return (
    <section
      aria-label="Test run"
      className="flex flex-col overflow-hidden rounded-md border border-border-default bg-surface-1"
    >
      <header className="flex shrink-0 items-center justify-between border-b border-border-default bg-surface-2 px-3 py-1.5">
        <h2 className="text-micro font-medium uppercase tracking-wide text-fg-default">
          Test run
        </h2>
        {onClose && (
          <IconButton label="Close test pane" size="sm" variant="ghost" onClick={onClose}>
            <Cross1Icon className="h-3.5 w-3.5" />
          </IconButton>
        )}
      </header>
      {runId === null ? (
        <TestLaunchForm launch={launch} vars={vars} target={target} onStarted={setRunId} />
      ) : (
        // key remounts the live subtree (snapshot fetch + WS) per run.
        <LiveTestRun key={runId} runId={runId} onReset={() => setRunId(null)} />
      )}
    </section>
  );
}

// ---------------------------------------------------------------------------
// Pre-launch state — var quick-inputs + the Test button
// ---------------------------------------------------------------------------

function TestLaunchForm({
  launch,
  vars,
  target,
  onStarted,
}: {
  launch: BotLaunch;
  vars: VarField[];
  /** What this pane launches — the same target, under the same key, as the
   *  launch form of this bot (lib/sendLaunch): a launch from either holds the
   *  other, and neither starts a second run while the first is pending. */
  target: LaunchTarget;
  onStarted: (runId: string) => void;
}) {
  const [values, setValues] = useState<Record<string, string>>(() =>
    // The baseline the payload is compared with (buildVarsPayload), so an
    // untouched field is exactly its default.
    Object.fromEntries(vars.map((f) => [f.name, defaultStringFor(f)])),
  );
  const [error, setError] = useState<string | null>(null);
  // Where to look before testing again, after a launch that drew no answer
  // about its run.
  const [unansweredRuns, setUnansweredRuns] = useState<string | null>(null);
  const [, navigate] = useLocation();
  const busy = useLaunchInFlight(target.key);
  // The pane is closed, or its page left, while a launch is in flight: its
  // answer is then announced by sendLaunch instead of shown here.
  const shown = useRef(true);
  useEffect(() => {
    shown.current = true;
    return () => {
      shown.current = false;
    };
  }, []);

  const start = async () => {
    setError(null);
    setUnansweredRuns(null);
    // Only what the operator touched (buildVarsPayload, the launch form's
    // rule). The pre-filled values are the session's picture of the bot's
    // defaults, and the launch is by id: an untouched one is the server's
    // to apply from the program as it is NOW — echoed back it would
    // override a default changed since, be refused for a var the program
    // dropped, and break a `${...}` default the server expands.
    const sendVars = buildVarsPayload(vars, values);
    const sent = await sendLaunch({
      target,
      onScreen: () => shown.current,
      navigate,
      send: () =>
        createRun({
          ...(launch.by === "bot" ? { bot_id: launch.bot } : { file_path: launch.file }),
          merge_into: "none",
          ...(sendVars ? { vars: sendVars } : {}),
        }),
    });
    if (!sent.onScreen) return;
    if (sent.outcome.kind === "started") {
      onStarted(sent.outcome.runID);
      return;
    }
    // What the failure means for the run is the helper's to say (launchNotice):
    // an outcome that said nothing of it is not a plain failure.
    if (sent.notice.kind === "unanswered") {
      setError(sent.notice.message);
      setUnansweredRuns(sent.notice.runsHref ?? null);
      return;
    }
    setError(sent.notice.kind === "not-started" ? sent.notice.message : errorMessage(sent.outcome.error));
  };

  return (
    <div className="flex flex-col gap-3 p-3">
      <p className="text-xs text-fg-muted">
        Launches a contained run of{" "}
        <code className="font-mono text-fg-default">
          {launch.by === "bot" ? launch.bot : launch.file}
        </code>{" "}
        —
        commits land on a storage branch only (no merge into your checked-out branch).
      </p>

      {vars.length > 0 && (
        <div className="flex flex-col gap-2">
          {vars.map((f) => (
            <label key={f.name} className="block">
              <span className="mb-1 flex items-baseline gap-1.5 text-xs text-fg-subtle">
                <span className="font-mono text-fg-default">{f.name}</span>
                <span className="text-caption">{f.type}</span>
              </span>
              <Input
                type="text"
                value={values[f.name] ?? ""}
                onChange={(e) =>
                  setValues((v) => ({ ...v, [f.name]: e.target.value }))
                }
                placeholder={f.default ? undefined : "required"}
                aria-label={`Var ${f.name}`}
                size="sm"
                className="font-mono"
              />
            </label>
          ))}
        </div>
      )}

      {error && (
        <InlineBanner
          tone={unansweredRuns ? "warning" : "danger"}
          title={unansweredRuns ? "No answer from the test launch" : "Test launch failed"}
        >
          {error}
          {unansweredRuns && (
            <>
              {" "}
              <Link href={unansweredRuns} className="underline underline-offset-2">
                Open its runs
              </Link>
            </>
          )}
        </InlineBanner>
      )}

      <div>
        <Button
          variant="primary"
          size="sm"
          onClick={() => void start()}
          disabled={busy}
          loading={busy}
        >
          {busy ? "Starting…" : "Test"}
        </Button>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// Live state — status header + transcript + operator chat
// ---------------------------------------------------------------------------

function LiveTestRun({ runId, onReset }: { runId: string; onReset: () => void }) {
  // Per-run registry store (same instance a RunView tab on this run
  // would use), scoped to this subtree via the Provider so the
  // transcript/chat hooks don't clash with any other mounted run.
  const store = useMemo(() => getOrCreateRunStore(runId), [runId]);
  return (
    <RunStoreProvider store={store}>
      <LiveTestRunInner runId={runId} onReset={onReset} />
    </RunStoreProvider>
  );
}

function LiveTestRunInner({ runId, onReset }: { runId: string; onReset: () => void }) {
  const { loadFailed, handleRetryLoad } = useRunSnapshot(runId);
  useRunWebSocket(runId);
  const status = useRunStore((s) => s.snapshot?.run.status);
  const terminal = status !== undefined && TERMINAL_STATUSES.has(status);

  if (loadFailed) {
    return (
      <div className="p-3">
        <InlineBanner
          tone="danger"
          title={loadFailed.status === 404 ? "Run not found" : "Failed to load run"}
        >
          <span className="break-words">
            {loadFailed.message || "The run could not be loaded from this server."}
          </span>
          <div className="mt-2 flex gap-2">
            <Button variant="secondary" size="sm" onClick={handleRetryLoad}>
              Retry
            </Button>
            <Button variant="ghost" size="sm" onClick={onReset}>
              New test
            </Button>
          </div>
        </InlineBanner>
      </div>
    );
  }

  return (
    <>
      <div className="flex shrink-0 flex-wrap items-center gap-2 border-b border-border-default px-3 py-1.5">
        {status ? (
          <Badge variant={STATUS_VARIANT[status] ?? "neutral"}>{labelForStatus(status)}</Badge>
        ) : (
          <span className="flex items-center gap-1.5 text-caption text-fg-muted">
            <Spinner size="sm" /> loading…
          </span>
        )}
        <span className="font-mono text-caption text-fg-subtle" title={runId}>
          {runId.slice(0, 8)}
        </span>
        <Link
          href={`/runs/${encodeURIComponent(runId)}`}
          className="text-caption text-accent-text hover:underline"
        >
          Open full run ↗
        </Link>
        <div className="ml-auto">
          <Button variant="ghost" size="sm" onClick={onReset}>
            New test
          </Button>
        </div>
      </div>

      <div className="h-[60vh] min-h-[320px] overflow-hidden">
        <RunConversationView runId={runId} />
      </div>

      <div className="shrink-0 border-t border-border-default bg-surface-0 px-3 py-2">
        <AgentChatboxInline runId={runId} compact disabled={terminal} embedded />
      </div>
    </>
  );
}
