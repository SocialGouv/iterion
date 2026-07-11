// TestRunPane — a self-contained, embeddable "create → test → iterate"
// pane (the Mistral-style side-by-side loop). Mounted by BotHome (the
// Test action) and by the BotBuilder right column after a successful
// create. It launches a CONTAINED run (merge_into: "none" — commits
// stay on a storage branch) and then streams the live transcript
// inline: RunConversationView + AgentChatboxInline over the per-run
// store, so no full RunView tab is needed.

import { useMemo, useState } from "react";
import { Cross1Icon } from "@radix-ui/react-icons";
import { Link } from "wouter";

import { createRun } from "@/api/runs";
import type { RunStatus } from "@/api/runs";
import type { VarField } from "@/api/types";
import RunConversationView from "@/components/Runs/conversation/RunConversationView";
import { literalToString } from "@/components/Runs/launchView/utils";
import { STATUS_VARIANT, labelForStatus } from "@/components/Runs/runStatusMeta";
import { useRunSnapshot } from "@/components/Runs/runView/useRunSnapshot";
import AgentChatboxInline from "@/components/shared/AgentChatboxInline";
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
import { RunStoreProvider, getOrCreateRunStore, useRunStore } from "@/store/run";

export interface TestRunPaneProps {
  /** Workspace-relative .bot path to run (botLaunchFile output). */
  file: string;
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

export default function TestRunPane({ file, vars = [], onClose }: TestRunPaneProps) {
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
        <TestLaunchForm file={file} vars={vars} onStarted={setRunId} />
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
  file,
  vars,
  onStarted,
}: {
  file: string;
  vars: VarField[];
  onStarted: (runId: string) => void;
}) {
  const [values, setValues] = useState<Record<string, string>>(() =>
    Object.fromEntries(vars.map((f) => [f.name, literalToString(f.default)])),
  );
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const start = async () => {
    setBusy(true);
    setError(null);
    try {
      const sendVars: Record<string, string> = {};
      for (const [k, v] of Object.entries(values)) {
        if (v.trim() !== "") sendVars[k] = v;
      }
      const res = await createRun({
        file_path: file,
        merge_into: "none",
        ...(Object.keys(sendVars).length > 0 ? { vars: sendVars } : {}),
      });
      onStarted(res.run_id);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="flex flex-col gap-3 p-3">
      <p className="text-xs text-fg-muted">
        Launches a contained run of <code className="font-mono text-fg-default">{file}</code> —
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
        <InlineBanner tone="danger" title="Test launch failed">
          {error}
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
