// Extracted from DeploymentRow.tsx to keep that file presentational.
// The traceability state machine + its per-state presentation: the part
// of the deployment row that decides whether a live URL may be read as
// a delivery.

import type { DeploymentTrace } from "@/api/runs";

// TraceabilityState is the operator-facing verdict on a delivery. Four
// causes, three of them named by the deployment-report contract:
//   traceable   — pushed + image from the repo + image names the commit
//   untraceable — the gate established the facts and one of them is false
//   unverified  — the gate ran and could NOT establish the facts
//                 (trace.verifiable === false); an environment fault, not
//                 a verdict on the deploy
//   unchecked   — no traceability gate ran at all
export type TraceabilityState =
  | "traceable"
  | "untraceable"
  | "unverified"
  | "unchecked";

export function traceabilityState(trace?: DeploymentTrace): TraceabilityState {
  if (!trace) return "unchecked";
  if (!trace.verifiable) return "unverified";
  if (trace.pushed && trace.image_from_repo && trace.built_from_head) {
    return "traceable";
  }
  return "untraceable";
}

// untraceableReasons names WHICH facts are false, so the row says what
// to fix rather than just that something is wrong. Only meaningful for
// the "untraceable" state (verifiable === true).
export function untraceableReasons(trace: DeploymentTrace): string[] {
  const reasons: string[] = [];
  if (!trace.pushed) reasons.push("not pushed");
  if (!trace.image_from_repo) reasons.push("image not from this repo");
  if (!trace.built_from_head) reasons.push("image doesn't name the commit");
  return reasons;
}

// Per-state chip presentation. The three tones are deliberately far
// apart: green = traced, red = traced and failing, blue = unknown.
// "unverified" and "unchecked" must never borrow the failure tone —
// neither is a verdict against the deploy.
export const TRACE_CHIP: Record<
  TraceabilityState,
  { label: string; className: string }
> = {
  traceable: {
    label: "traceable",
    className: "bg-success-soft text-success-fg",
  },
  untraceable: {
    label: "not traceable",
    className: "bg-danger-soft text-danger-fg",
  },
  unverified: {
    label: "traceability unverified",
    className: "bg-info-soft text-info-fg",
  },
  unchecked: {
    label: "traceability not checked",
    className: "bg-surface-2 text-fg-muted border border-border-default",
  },
};

export const TRACE_TOOLTIP: Record<TraceabilityState, string> = {
  traceable:
    "The commits are on a remote branch, the running image is published under this repo, and it names the deployed commit.",
  untraceable:
    "The URL may answer, but this delivery cannot be traced back to reviewable source.",
  unverified:
    "The traceability gate could not establish the facts (e.g. git unreachable). This is an environment fault, NOT a verdict on the deploy — judge it yourself.",
  unchecked:
    "This run reported no traceability gate, so nothing checked whether the delivery traces back to the repository.",
};
