// Extracted from LaunchView.tsx to keep that file focused.
// WorktreeTargetSummary renders the one-line "Commits → branch · FF→ target"
// summary above the worktree finalization fields, so the operator sees
// where their commits will land without parsing four input fields.

export default function WorktreeTargetSummary({
  branchName,
  mergeInto,
  cloud = false,
}: {
  branchName: string;
  mergeInto: string;
  /** Cloud runs FF into the target repo's default branch inside the
   *  runner pod — there is no operator-side "current branch" to name. */
  cloud?: boolean;
}) {
  const branch = branchName || "iterion/run/<auto>";
  const skipMerge = mergeInto === "none";
  const target =
    mergeInto && mergeInto !== "current"
      ? mergeInto
      : cloud
        ? "target repo default branch"
        : "current branch";
  return (
    <div className="text-micro text-fg-muted bg-surface-2 border border-border-default rounded px-2 py-1.5">
      <span className="text-fg-subtle">Commits → </span>
      <code className="font-mono text-fg-default">{branch}</code>
      <span className="text-fg-subtle"> · </span>
      {skipMerge ? (
        <span className="text-fg-default">Branch only — no fast-forward</span>
      ) : (
        <>
          <span className="text-fg-subtle">FF→ </span>
          <code className="font-mono text-fg-default">{target}</code>
        </>
      )}
    </div>
  );
}
