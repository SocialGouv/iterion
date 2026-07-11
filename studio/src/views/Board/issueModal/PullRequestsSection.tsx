import { useEffect, useState } from "react";

import {
  createIssuePull,
  getIssuePullCI,
  listIssuePulls,
  mergeIssuePull,
  type CIRun,
  type MergeMethod,
  type NativeIssue,
  type PullRef,
} from "@/api/native";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Checkbox } from "@/components/ui/Checkbox";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { useConfirm, type ConfirmOptions } from "@/hooks/useConfirm";
import { useServerInfoStore } from "@/store/serverInfo";
import { useUIStore } from "@/store/ui";

import { ciRunVariant, ciTone, prStateVariant } from "./ci";

// PullRequestsSection lists the forge pull/merge requests linked to a card,
// each with a compact CI status indicator and an expandable run list. Read-
// only. Cloud-mode only, and rendered only when the card is forge-linked
// (external.repo present) — so a plain native card shows nothing.
export function PullRequestsSection({ issue }: { issue: NativeIssue }) {
  const mode = useServerInfoStore((s) => s.info?.mode);
  const addToast = useUIStore((s) => s.addToast);
  const [pulls, setPulls] = useState<PullRef[] | null>(null);
  const [creating, setCreating] = useState(false);
  const loadAction = useAsyncAction();
  const { confirm, dialog } = useConfirm();

  const forgeLinked = !!issue.external?.repo;
  const eligible = mode === "cloud" && forgeLinked;

  const refresh = async () => {
    await loadAction.run(async () => {
      setPulls(await listIssuePulls(issue.id));
    });
  };

  useEffect(() => {
    if (!eligible) {
      setPulls(null);
      return;
    }
    void loadAction.run(async () => {
      setPulls(await listIssuePulls(issue.id));
    });
    // Re-fetch only when the target issue changes; loadAction is stable.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [issue.id, eligible]);

  // Hide entirely for non-cloud / unlinked cards. A forge-linked card with no
  // PRs still renders so the operator can open one.
  if (!eligible) return null;

  const pullList = pulls ?? [];

  return (
    <div className="rounded border border-border-default bg-surface-1 p-2 space-y-2">
      {dialog}
      <div className="flex items-center justify-between gap-2">
        <div className="text-micro uppercase tracking-wide text-fg-subtle">Pull requests</div>
        <button
          type="button"
          onClick={() => setCreating((v) => !v)}
          className="text-micro text-accent-text hover:underline"
        >
          {creating ? "Cancel" : "+ Open PR"}
        </button>
      </div>
      {loadAction.busy && (
        <p className="text-xs text-fg-subtle italic">Loading pull requests…</p>
      )}
      {loadAction.error && (
        <InlineBanner tone="danger" layout="inline">
          {loadAction.error}
        </InlineBanner>
      )}
      {creating && (
        <CreatePullForm
          issueID={issue.id}
          onCreated={async (pr) => {
            setCreating(false);
            addToast(`Opened PR #${pr.number}`, "success");
            await refresh();
          }}
        />
      )}
      {!loadAction.busy && pullList.length === 0 && !loadAction.error && !creating && (
        <p className="text-xs text-fg-subtle">No pull requests linked to this card yet.</p>
      )}
      {pullList.map((pr) => (
        <PullRow
          key={pr.number}
          issueID={issue.id}
          pr={pr}
          confirm={confirm}
          onMerged={async (merged) => {
            addToast(`Merged PR #${merged.number}`, "success");
            await refresh();
          }}
        />
      ))}
    </div>
  );
}

// CreatePullForm is a minimal source→target branch form that opens a PR for a
// forge-linked card (it reuses the card's connection + repo server-side).
function CreatePullForm({
  issueID,
  onCreated,
}: {
  issueID: string;
  onCreated: (pr: PullRef) => void | Promise<void>;
}) {
  const [source, setSource] = useState("");
  const [target, setTarget] = useState("");
  const [draft, setDraft] = useState(false);
  const action = useAsyncAction();

  const submit = async () => {
    if (!source.trim() || !target.trim()) {
      action.setError("Source and target branch are required.");
      return;
    }
    const pr = await action.run(() =>
      createIssuePull(issueID, {
        source_branch: source.trim(),
        target_branch: target.trim(),
        draft,
      }),
    );
    if (pr) await onCreated(pr);
  };

  return (
    <div className="rounded border border-border-subtle bg-surface-2 p-2 space-y-2">
      <div className="grid grid-cols-[auto_1fr] items-center gap-2">
        <label className="text-micro text-fg-subtle">Source</label>
        <Input
          size="sm"
          value={source}
          onChange={(e) => setSource(e.target.value)}
          placeholder="feature/my-branch"
        />
        <label className="text-micro text-fg-subtle">Target</label>
        <Input
          size="sm"
          value={target}
          onChange={(e) => setTarget(e.target.value)}
          placeholder="main"
        />
      </div>
      <label className="inline-flex items-center gap-2">
        <Checkbox checked={draft} onChange={(e) => setDraft(e.target.checked)} />
        <span className="text-micro text-fg-muted">Open as draft</span>
      </label>
      {action.error && (
        <InlineBanner tone="danger" layout="inline">
          {action.error}
        </InlineBanner>
      )}
      <Button
        size="sm"
        onClick={() => void submit()}
        loading={action.busy}
        disabled={action.busy}
      >
        Open PR
      </Button>
    </div>
  );
}

// PullRow renders one PR with its CI dot, a Merge action for open PRs, and an
// expandable run list (current runs + recent history, lazily fetched on first
// expand).
function PullRow({
  issueID,
  pr,
  confirm,
  onMerged,
}: {
  issueID: string;
  pr: PullRef;
  confirm: (o: ConfirmOptions) => Promise<boolean>;
  onMerged: (merged: PullRef) => void | Promise<void>;
}) {
  const [expanded, setExpanded] = useState(false);
  const [runs, setRuns] = useState<CIRun[] | null>(null);
  const [ciState, setCIState] = useState<string>("");
  const [method, setMethod] = useState<MergeMethod>("merge");
  const ciAction = useAsyncAction();
  const mergeAction = useAsyncAction();

  const loadCI = async () => {
    await ciAction.run(async () => {
      const { status, history } = await getIssuePullCI(issueID, pr.number);
      setCIState(status.state);
      // Current runs first, then recent history; de-dupe is the server's job.
      setRuns([...(status.runs ?? []), ...(history ?? [])]);
    });
  };

  const toggle = () => {
    const next = !expanded;
    setExpanded(next);
    if (next && runs === null && !ciAction.busy) void loadCI();
  };

  // Open PRs (not merged/closed/draft) can be merged. The forge enforces the
  // real merge gate (CI, approvals) — this is the operator affordance.
  const mergeable = ["open", "opened"].includes(pr.state.toLowerCase()) && !pr.draft;

  const doMerge = async () => {
    const ok = await confirm({
      title: `Merge PR #${pr.number}?`,
      message: `Merge ${pr.source_branch} → ${pr.target_branch} using "${method}"? This cannot be undone.`,
      confirmLabel: "Merge",
    });
    if (!ok) return;
    const merged = await mergeAction.run(() =>
      mergeIssuePull(issueID, pr.number, { method }),
    );
    if (merged) await onMerged(merged);
  };

  return (
    <div className="border-t border-border-subtle pt-1.5 first:border-t-0 first:pt-0">
      <div className="flex items-center gap-2 text-xs">
        <CIDot state={ciState} />
        <a
          href={pr.url}
          target="_blank"
          rel="noreferrer"
          className="font-medium text-accent-text hover:underline truncate"
          title={pr.title}
        >
          #{pr.number} {pr.title}
        </a>
        <Badge variant={prStateVariant(pr.state)} size="sm">
          {pr.draft ? "draft" : pr.state}
        </Badge>
      </div>
      <div className="mt-0.5 flex items-center gap-2 text-micro text-fg-subtle">
        <span className="font-mono truncate">
          {pr.source_branch} → {pr.target_branch}
        </span>
        <button
          type="button"
          onClick={toggle}
          className="ml-auto text-accent-text hover:underline"
        >
          {expanded ? "Hide CI" : "CI"}
        </button>
      </div>
      {mergeable && (
        <div className="mt-1 flex items-center gap-2">
          <Select
            size="sm"
            fit
            value={method}
            onChange={(e) => setMethod(e.target.value as MergeMethod)}
            disabled={mergeAction.busy}
            aria-label="Merge method"
          >
            <option value="merge">merge</option>
            <option value="squash">squash</option>
            <option value="rebase">rebase</option>
          </Select>
          <Button
            size="sm"
            onClick={() => void doMerge()}
            loading={mergeAction.busy}
            disabled={mergeAction.busy}
          >
            Merge
          </Button>
        </div>
      )}
      {mergeAction.error && (
        <InlineBanner tone="danger" layout="inline" className="mt-1">
          {mergeAction.error}
        </InlineBanner>
      )}
      {expanded && (
        <div className="mt-1 space-y-1">
          {ciAction.busy && (
            <p className="text-micro text-fg-subtle italic">Loading CI runs…</p>
          )}
          {ciAction.error && (
            <InlineBanner tone="danger" layout="inline">
              {ciAction.error}
            </InlineBanner>
          )}
          {runs && runs.length === 0 && !ciAction.busy && (
            <p className="text-micro text-fg-subtle">No CI runs reported.</p>
          )}
          {(runs ?? []).map((run, i) => (
            <div key={`${run.name}-${run.sha}-${i}`} className="flex items-center gap-2 text-micro">
              <Badge variant={ciRunVariant(run)} size="sm">
                {run.conclusion || run.status}
              </Badge>
              {run.url ? (
                <a
                  href={run.url}
                  target="_blank"
                  rel="noreferrer"
                  className="text-accent-text hover:underline truncate"
                >
                  {run.name}
                </a>
              ) : (
                <span className="text-fg-default truncate">{run.name}</span>
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}

// CIDot renders the aggregate CI state as a coloured dot:
// success=green, failed=red, running/pending=amber, unknown=grey.
function CIDot({ state }: { state: string }) {
  const tone = ciTone(state);
  const cls: Record<typeof tone, string> = {
    success: "bg-success",
    danger: "bg-danger",
    warning: "bg-warning",
    neutral: "bg-fg-subtle",
  };
  return (
    <span
      className={`inline-block h-2 w-2 rounded-full shrink-0 ${cls[tone]}`}
      title={state ? `CI: ${state}` : "CI status unknown"}
      aria-label={state ? `CI ${state}` : "CI status unknown"}
    />
  );
}
