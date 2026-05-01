import { useEffect, useMemo, useState } from "react";

import type { Artifact, ArtifactSummary } from "@/api/runs";
import { getArtifact } from "@/api/runs";

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
  // dp[i][j] = LCS length of a[0..i) and b[0..j).
  const dp: number[][] = Array.from({ length: m + 1 }, () =>
    new Array(n + 1).fill(0),
  );
  for (let i = 1; i <= m; i++) {
    for (let j = 1; j <= n; j++) {
      if (a[i - 1] === b[j - 1]) dp[i]![j] = dp[i - 1]![j - 1]! + 1;
      else dp[i]![j] = Math.max(dp[i - 1]![j]!, dp[i]![j - 1]!);
    }
  }
  const out: LineChange[] = [];
  let i = m;
  let j = n;
  while (i > 0 && j > 0) {
    if (a[i - 1] === b[j - 1]) {
      out.push({ kind: "ctx", text: a[i - 1]! });
      i--;
      j--;
    } else if (dp[i - 1]![j]! >= dp[i]![j - 1]!) {
      out.push({ kind: "del", text: a[i - 1]! });
      i--;
    } else {
      out.push({ kind: "add", text: b[j - 1]! });
      j--;
    }
  }
  while (i > 0) {
    out.push({ kind: "del", text: a[i - 1]! });
    i--;
  }
  while (j > 0) {
    out.push({ kind: "add", text: b[j - 1]! });
    j--;
  }
  return out.reverse();
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
  const [fromBody, setFromBody] = useState<Artifact | null>(null);
  const [toBody, setToBody] = useState<Artifact | null>(null);
  const [error, setError] = useState<string | null>(null);

  // Reset selection when the underlying versions list changes (new
  // execution selected, or fresh artifact landed mid-run).
  useEffect(() => {
    setFromV(previous);
    setToV(latest);
  }, [previous, latest]);

  useEffect(() => {
    let cancelled = false;
    setError(null);
    Promise.all([
      getArtifact(runId, nodeId, fromV),
      getArtifact(runId, nodeId, toV),
    ])
      .then(([a, b]) => {
        if (cancelled) return;
        setFromBody(a);
        setToBody(b);
      })
      .catch((e) => {
        if (!cancelled) setError((e as Error).message);
      });
    return () => {
      cancelled = true;
    };
  }, [runId, nodeId, fromV, toV]);

  const diff = useMemo(() => {
    if (!fromBody || !toBody) return null;
    if (fromV === toV) return null;
    const fromLines = JSON.stringify(fromBody.data, null, 2).split("\n");
    const toLines = JSON.stringify(toBody.data, null, 2).split("\n");
    return diffLines(fromLines, toLines);
  }, [fromBody, toBody, fromV, toV]);

  if (error) {
    return <div className="text-danger-fg text-[10px]">Diff failed: {error}</div>;
  }

  return (
    <>
      <div className="flex items-center gap-2 mb-2 text-[10px]">
        <label>
          from{" "}
          <select
            value={fromV}
            onChange={(e) => setFromV(Number(e.target.value))}
            className="bg-surface-2 px-1 py-0.5 rounded font-mono"
          >
            {sorted.map((v) => (
              <option key={v.version} value={v.version}>
                v{v.version}
              </option>
            ))}
          </select>
        </label>
        <span className="text-fg-subtle">→</span>
        <label>
          to{" "}
          <select
            value={toV}
            onChange={(e) => setToV(Number(e.target.value))}
            className="bg-surface-2 px-1 py-0.5 rounded font-mono"
          >
            {sorted.map((v) => (
              <option key={v.version} value={v.version}>
                v{v.version}
              </option>
            ))}
          </select>
        </label>
      </div>
      {fromV === toV ? (
        <pre className="bg-surface-2 rounded p-2 text-[10px] font-mono whitespace-pre-wrap break-all max-h-[60vh] overflow-auto">
          {toBody ? JSON.stringify(toBody.data, null, 2) : "Loading…"}
        </pre>
      ) : !diff ? (
        <div className="text-fg-subtle text-[10px]">Loading diff…</div>
      ) : (
        <pre className="rounded font-mono text-[10px] max-h-[60vh] overflow-auto border border-border-default">
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
