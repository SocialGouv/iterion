import { type BadgeVariant } from "@/components/ui/Badge";
import { type CIRun } from "@/api/native";

export type CITone = "success" | "danger" | "warning" | "neutral";

// ciTone maps a forge CI aggregate state to a semantic tone. Covers the
// common GitHub/GitLab/Forgejo vocabularies (success/passed, failure/error,
// running/pending/in_progress).
export function ciTone(state: string): CITone {
  const s = state.toLowerCase();
  if (["success", "passed", "completed", "ok"].includes(s)) return "success";
  if (["failure", "failed", "error", "cancelled", "canceled"].includes(s)) return "danger";
  if (["running", "pending", "in_progress", "queued", "waiting"].includes(s)) return "warning";
  return "neutral";
}

// ciRunVariant prefers the run's conclusion (terminal) over its status
// (lifecycle) when colouring the per-run badge. CITone is a subset of
// BadgeVariant, so the tone doubles as the variant.
export function ciRunVariant(run: CIRun): BadgeVariant {
  return ciTone(run.conclusion || run.status);
}

// prStateVariant maps a PR state to a badge variant (open=success,
// merged=accent, closed=neutral).
export function prStateVariant(state: string): BadgeVariant {
  const s = state.toLowerCase();
  if (s === "merged") return "accent";
  if (s === "open" || s === "opened") return "success";
  return "neutral";
}
