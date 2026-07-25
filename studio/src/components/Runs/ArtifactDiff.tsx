import { errorMessage } from "@/lib/errorHints";
import { useEffect, useMemo, useState } from "react";
import { keepPreviousData, useQuery } from "@tanstack/react-query";

import type { ArtifactSummary } from "@/api/runs";
import { getArtifact } from "@/api/runs";
import { CopyButton, Select, Spinner } from "@/components/ui";

interface Props {
  runId: string;
  nodeId: string;
  versions: ArtifactSummary[];
}

interface LineChange {
  kind: "ctx" | "add" | "del";
  text: string;
}

/** Diff two pretty-printed JSON blobs line-by-line via classical LCS.
 *  Myers diff is overkill for the artifact sizes we see in practice
 *  (typically < 1k lines) and the result here is easy to read in a
 *  narrow inspector panel. */
function diffLines(a: string[], b: string[]): LineChange[] {
  const m = a.length;
  const n = b.length;
  // Flat backing array for the LCS table. We hide indexing behind
  // get/set helpers whose return types are `number`, so the LCS body
  // stays free of `!` non-null assertions; the helpers do the single
  // ?? 0 fallback the noUncheckedIndexedAccess rule wants.
  const W = n + 1;
  const dp = new Array<number>((m + 1) * W).fill(0);
  const dpGet = (i: number, j: number): number => dp[i * W + j] ?? 0;
  const dpSet = (i: number, j: number, v: number): void => {
    dp[i * W + j] = v;
  };
  const ai = (k: number): string => a[k] ?? "";
  const bi = (k: number): string => b[k] ?? "";
  for (let i = 1; i <= m; i++) {
    for (let j = 1; j <= n; j++) {
      if (ai(i - 1) === bi(j - 1)) dpSet(i, j, dpGet(i - 1, j - 1) + 1);
      else dpSet(i, j, Math.max(dpGet(i - 1, j), dpGet(i, j - 1)));
    }
  }
  const out: LineChange[] = [];
  let i = m;
  let j = n;
  while (i > 0 && j > 0) {
    const av = ai(i - 1);
    const bv = bi(j - 1);
    if (av === bv) {
      out.push({ kind: "ctx", text: av });
      i--;
      j--;
    } else if (dpGet(i - 1, j) >= dpGet(i, j - 1)) {
      out.push({ kind: "del", text: av });
      i--;
    } else {
      out.push({ kind: "add", text: bv });
      j--;
    }
  }
  while (i > 0) {
    out.push({ kind: "del", text: ai(i - 1) });
    i--;
  }
  while (j > 0) {
    out.push({ kind: "add", text: bi(j - 1) });
    j--;
  }
  return out.reverse();
}

/** Coerce a verdict field that may be a string[] or a single string
 *  into a clean list, dropping blank entries. */
function asStringList(v: unknown): string[] {
  if (Array.isArray(v))
    return v.map((x) => String(x)).filter((s) => s.trim() !== "");
  if (typeof v === "string" && v.trim() !== "") return [v];
  return [];
}

function firstString(d: Record<string, unknown>, keys: string[]): string {
  for (const k of keys) {
    if (typeof d[k] === "string" && (d[k] as string).trim() !== "")
      return (d[k] as string).trim();
  }
  return "";
}

/** A reviewer/judge artifact is verdict-shaped when it carries any of the
 *  recognised decision fields. Anything else falls through to the raw JSON
 *  view unchanged. */
function isVerdictShaped(data: unknown): data is Record<string, unknown> {
  if (!data || typeof data !== "object" || Array.isArray(data)) return false;
  const d = data as Record<string, unknown>;
  return [
    "approved",
    "blockers",
    "fix_plan",
    "verdict",
    "rationale",
    "confidence",
    "passed",
    "decision",
  ].some((k) => k in d);
}

/** VerdictCard renders the human-relevant parts of a reviewer/judge
 *  verdict — the approval state, the blockers (why it refused), and the
 *  fix plan — so an operator doesn't have to read raw JSON to learn what
 *  the agents replied. Raw JSON still renders below for full detail. */
function VerdictCard({ data }: { data: Record<string, unknown> }) {
  const blockers = asStringList(data.blockers);
  const fixPlan = firstString(data, ["fix_plan"]);
  const rationale = firstString(data, [
    "rationale",
    "summary",
    "reason",
    "notes",
  ]);
  const confidence = firstString(data, ["confidence"]);
  const family = firstString(data, ["family"]);

  let approved: boolean | null = null;
  for (const k of ["approved", "passed", "pass"]) {
    if (typeof data[k] === "boolean") {
      approved = data[k] as boolean;
      break;
    }
  }
  if (approved === null) {
    const verdictStr = firstString(data, [
      "verdict",
      "decision",
      "status",
    ]).toLowerCase();
    if (/approv|pass|accept|lgtm/.test(verdictStr)) approved = true;
    else if (/reject|block|fail|den|chang/.test(verdictStr)) approved = false;
  }
  if (approved === null && blockers.length > 0) approved = false;

  return (
    <div className="mb-3 rounded border border-border-default bg-surface-1 p-2 space-y-2 text-micro">
      <div className="flex items-center gap-2 flex-wrap">
        {approved === true && (
          <span className="rounded px-1.5 py-0.5 bg-success-soft text-success-fg font-medium">
            ✓ approved
          </span>
        )}
        {approved === false && (
          <span className="rounded px-1.5 py-0.5 bg-danger-soft text-danger-fg font-medium">
            ✗ changes requested
          </span>
        )}
        {confidence && (
          <span className="rounded px-1.5 py-0.5 bg-surface-2 text-fg-muted">
            confidence: {confidence}
          </span>
        )}
        {family && (
          <span className="rounded px-1.5 py-0.5 bg-surface-2 text-fg-muted">
            family: {family}
          </span>
        )}
      </div>
      {rationale && (
        <div>
          <div className="text-fg-muted mb-0.5">rationale</div>
          <p className="whitespace-pre-wrap text-fg-default">{rationale}</p>
        </div>
      )}
      {blockers.length > 0 && (
        <div>
          <div className="text-danger-fg mb-0.5">
            blockers ({blockers.length})
          </div>
          <ul className="list-disc pl-4 space-y-0.5 text-fg-default">
            {blockers.map((b, i) => (
              <li key={i} className="whitespace-pre-wrap">
                {b}
              </li>
            ))}
          </ul>
        </div>
      )}
      {fixPlan && (
        <div>
          <div className="text-fg-muted mb-0.5">fix plan</div>
          <p className="whitespace-pre-wrap text-fg-default">{fixPlan}</p>
        </div>
      )}
    </div>
  );
}

/** A plan artifact is "plan-shaped" when it carries a `plan` or `text`
 *  string field — e.g. feature_dev's published plan, `{text: "## …", …}`.
 *  Verdict-shaped data goes to VerdictCard; structured data (tool
 *  manifests) has neither field and falls through to the raw JSON view. */
function isPlanShaped(data: unknown): data is Record<string, unknown> {
  if (!data || typeof data !== "object" || Array.isArray(data)) return false;
  if (isVerdictShaped(data)) return false;
  return firstString(data as Record<string, unknown>, ["plan", "text"]) !== "";
}

/** PlanCard renders the prose body (`plan` or `text`) with newlines
 *  preserved, so a published plan reads like a plan, not escaped JSON.
 *  Metadata keys are omitted; the full record is in the raw JSON below. */
function PlanCard({ data }: { data: Record<string, unknown> }) {
  const body = firstString(data, ["plan", "text"]);
  if (!body) return null;
  return (
    <div className="mb-3 rounded border border-border-default bg-surface-1 p-2 text-micro">
      <div className="text-fg-muted mb-1">plan</div>
      <p className="whitespace-pre-wrap text-fg-default">{body}</p>
    </div>
  );
}

export default function ArtifactDiff({ runId, nodeId, versions }: Props) {
  // Sort once: highest first so the default selection is "previous vs
  // latest" — the most useful diff for loop iterations or retries.
  const sorted = useMemo(
    () => [...versions].sort((a, b) => b.version - a.version),
    [versions],
  );

  const latest = sorted[0]?.version ?? 1;
  const previous = sorted[1]?.version ?? latest;

  const [fromV, setFromV] = useState<number>(previous);
  const [toV, setToV] = useState<number>(latest);

  // Reset selection when the underlying versions list changes (new
  // execution selected, or fresh artifact landed mid-run).
  useEffect(() => {
    setFromV(previous);
    setToV(latest);
  }, [previous, latest]);

  // keepPreviousData keeps the previously selected version's body (and
  // its diff) on screen while the newly selected one loads, instead of
  // flashing the panel back to a spinner on every version switch.
  const fromQuery = useQuery({
    queryKey: ["run-artifact", runId, nodeId, fromV],
    queryFn: () => getArtifact(runId, nodeId, fromV),
    placeholderData: keepPreviousData,
  });
  const toQuery = useQuery({
    queryKey: ["run-artifact", runId, nodeId, toV],
    queryFn: () => getArtifact(runId, nodeId, toV),
    placeholderData: keepPreviousData,
  });
  const fromBody = fromQuery.data ?? null;
  const toBody = toQuery.data ?? null;
  const loadError = fromQuery.error ?? toQuery.error;
  const error = loadError ? errorMessage(loadError) : null;

  const diff = useMemo(() => {
    if (!fromBody || !toBody) return null;
    if (fromV === toV) return null;
    const fromLines = JSON.stringify(fromBody.data, null, 2).split("\n");
    const toLines = JSON.stringify(toBody.data, null, 2).split("\n");
    return diffLines(fromLines, toLines);
  }, [fromBody, toBody, fromV, toV]);

  if (error) {
    return <div className="text-danger-fg text-caption">Diff failed: {error}</div>;
  }

  return (
    <>
      {toBody && isVerdictShaped(toBody.data) && (
        <VerdictCard data={toBody.data as Record<string, unknown>} />
      )}
      {toBody && isPlanShaped(toBody.data) && (
        <PlanCard data={toBody.data as Record<string, unknown>} />
      )}
      <div className="flex items-center gap-2 mb-2 text-caption">
        <span className="flex items-center gap-1">
          from{" "}
          <Select
            aria-label="Compare from version"
            value={fromV}
            onChange={(e) => setFromV(Number(e.target.value))}
            className="font-mono w-auto"
          >
            {sorted.map((v) => (
              <option key={v.version} value={v.version}>
                v{v.version}
              </option>
            ))}
          </Select>
        </span>
        <span className="text-fg-subtle">→</span>
        <span className="flex items-center gap-1">
          to{" "}
          <Select
            aria-label="Compare to version"
            value={toV}
            onChange={(e) => setToV(Number(e.target.value))}
            className="font-mono w-auto"
          >
            {sorted.map((v) => (
              <option key={v.version} value={v.version}>
                v{v.version}
              </option>
            ))}
          </Select>
        </span>
      </div>
      {fromV === toV ? (
        <div className="relative">
          {toBody && (
            <div className="absolute right-1 top-1 z-[var(--z-canvas)]">
              <CopyButton value={JSON.stringify(toBody.data, null, 2)} />
            </div>
          )}
          {toBody ? (
            <pre className="bg-surface-2 rounded p-2 text-caption font-mono whitespace-pre-wrap break-all max-h-[60vh] overflow-auto">
              {JSON.stringify(toBody.data, null, 2)}
            </pre>
          ) : (
            <div className="flex items-center justify-center rounded bg-surface-2 p-4">
              <Spinner size="sm" label="Loading artifact" />
            </div>
          )}
        </div>
      ) : !diff ? (
        <div className="flex items-center gap-1.5 text-fg-subtle text-caption">
          <Spinner size="xs" /> Loading diff…
        </div>
      ) : (
        <pre className="rounded font-mono text-caption max-h-[60vh] overflow-auto border border-border-default">
          {diff.map((line, i) => (
            <div
              key={i}
              className={
                line.kind === "add"
                  ? "bg-success-soft text-success-fg px-1"
                  : line.kind === "del"
                  ? "bg-danger-soft text-danger-fg px-1"
                  : "px-1 text-fg-subtle"
              }
            >
              <span className="select-none mr-1">
                {line.kind === "add" ? "+" : line.kind === "del" ? "-" : " "}
              </span>
              {line.text || " "}
            </div>
          ))}
        </pre>
      )}
    </>
  );
}
