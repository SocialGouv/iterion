import { useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";

import {
  createIssuePull,
  getIssuePullCI,
  listIssuePulls,
  mergeIssuePull,
  type MergeMethod,
  type NativeIssue,
  type PullRef,
} from "@/api/native";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Checkbox } from "@/components/ui/Checkbox";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Input } from "@/components/ui/Input";
import { LiveDot } from "@/components/ui/LiveDot";
import { Select } from "@/components/ui/Select";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { useConfirm, type ConfirmOptions } from "@/hooks/useConfirm";
import { errorMessage } from "@/lib/errorHints";
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
  const [creating, setCreating] = useState(false);
  const { confirm, dialog } = useConfirm();

  const forgeLinked = !!issue.external?.repo;
  const eligible = mode === "cloud" && forgeLinked;

  const queryClient = useQueryClient();
  const pullsQuery = useQuery<PullRef[]>({
    queryKey: ["issue-pulls", issue.id],
    queryFn: () => listIssuePulls(issue.id),
    enabled: eligible,
  });
  // isFetching (not isLoading) so the post-create / post-merge re-pull
  // shows the same loading line as the initial load; the error hides
  // while a reload is in flight.
  const loading = pullsQuery.isFetching;
  const loadError =
    pullsQuery.error && !loading ? errorMessage(pullsQuery.error) : null;

  const refresh = () =>
    queryClient.invalidateQueries({ queryKey: ["issue-pulls", issue.id] });

  // Hide entirely for non-cloud / unlinked cards. A forge-linked card with no
  // PRs still renders so the operator can open one.
  if (!eligible) return null;

  const pullList = pullsQuery.data ?? [];

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
      {loading && (
        <p className="text-xs text-fg-subtle italic">Loading pull requests…</p>
      )}
      {loadError && (
        <InlineBanner tone="danger" layout="inline">
          {loadError}
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
      {!loading && pullList.length === 0 && !loadError && !creating && (
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
  // Latches true on the first expand: the CI query fetches once, then
  // collapse / re-expand just shows the cached list.
  const [ciRequested, setCIRequested] = useState(false);
  const [method, setMethod] = useState<MergeMethod>("merge");
  const mergeAction = useAsyncAction();

  const ciQuery = useQuery({
    queryKey: ["issue-pull-ci", issueID, pr.number],
    queryFn: () => getIssuePullCI(issueID, pr.number),
    enabled: ciRequested,
  });
  const ciState = ciQuery.data?.status.state ?? "";
  // Current runs first, then recent history; de-dupe is the server's job.
  const runs = useMemo(
    () =>
      ciQuery.data
        ? [...(ciQuery.data.status.runs ?? []), ...(ciQuery.data.history ?? [])]
        : null,
    [ciQuery.data],
  );
  const ciLoading = ciQuery.isLoading;
  const ciError = ciQuery.error ? errorMessage(ciQuery.error) : null;

  const toggle = () => {
    setExpanded((v) => !v);
    setCIRequested(true);
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
          {ciLoading && (
            <p className="text-micro text-fg-subtle italic">Loading CI runs…</p>
          )}
          {ciError && (
            <InlineBanner tone="danger" layout="inline">
              {ciError}
            </InlineBanner>
          )}
          {runs && runs.length === 0 && !ciLoading && (
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

// CIDot renders the aggregate CI state as a steady coloured dot:
// success=green, failed=red, running/pending=amber, unknown=grey.
// CITone names are a subset of LiveDot tones, so the tone passes through.
function CIDot({ state }: { state: string }) {
  return (
    <span
      className="inline-flex shrink-0"
      title={state ? `CI: ${state}` : "CI status unknown"}
    >
      <LiveDot
        tone={ciTone(state)}
        size="md"
        pulse={false}
        label={state ? `CI ${state}` : "CI status unknown"}
      />
    </span>
  );
}
