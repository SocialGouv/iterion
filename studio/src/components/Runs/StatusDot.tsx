import type { RunFileStatus } from "@/api/runs";

// VSCode-style status palette for run-file / commit-file rows. Compared to
// the flat-list StatusBadge, the dot is leaner (no filled background) so the
// row chrome doesn't compete with the line-count column on the right. The
// letter form (M/A/D/R, "U" for untracked) is what the eye reads; the glyph
// carries a text alternative, so the WORD is what a screen reader hears — a
// letter read aloud, or the raw "??" code, tells nobody anything. Shared by
// FilesPanel + CommitDetailDialog, which rendered byte-identical copies.
const STATUS_WORD: Record<string, string> = {
  M: "modified",
  A: "added",
  D: "deleted",
  R: "renamed",
  "??": "untracked",
};

const STATUS_CLASS: Record<string, string> = {
  M: "text-warning-fg",
  A: "text-success-fg",
  D: "text-danger-fg",
  R: "text-info-fg",
  "??": "text-fg-muted",
};

export function StatusDot({ status }: { status: RunFileStatus }) {
  const cls = STATUS_CLASS[status] ?? "text-fg-muted";
  const letter = status === "??" ? "U" : status;
  return (
    <span
      // A glyph with a text alternative: aria-label on a roleless span is
      // prohibited (axe aria-prohibited-attr), same shape as ui/HelpHint.
      role="img"
      className={`inline-flex h-3 w-3 shrink-0 items-center justify-center text-caption font-bold leading-none ${cls}`}
      aria-label={`status ${STATUS_WORD[status] ?? status}`}
    >
      {letter}
    </span>
  );
}
