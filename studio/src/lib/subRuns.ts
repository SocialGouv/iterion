// Pure helpers for the run console's sub-run surface: the child runs a
// parent run's subbot nodes spawn (one child run per subbot execution;
// fan_out_each × subbot = N parallel children). The subbot frame's tab
// strip (SubbotRunFrame) and the compact card's chip row build on
// these. All pure data — no React, no fetching — so they unit-test in
// node env.

import type { RunStatus, RunSummary, WireWorkflow } from "@/api/runs";

// Bucket key for children that cannot be attributed to a specific
// subbot node: parent_node_id absent (legacy children, router shards)
// AND the parent workflow has zero or 2+ subbot nodes.
const UNATTRIBUTED_NODE = "";

// subbotNodeIds lists the parent workflow's subbot-kind node ids, in
// declaration order. Empty when the IR hasn't loaded yet.
function subbotNodeIds(wf: WireWorkflow | null): string[] {
  if (!wf) return [];
  return wf.nodes.filter((n) => n.kind === "subbot").map((n) => n.id);
}

// groupChildrenByNode buckets a run's children by the parent IR node
// that spawned them (contract C3's parent_node_id). Children without
// the field fall back to the single subbot node when the workflow has
// exactly one (the unambiguous case), else to the UNATTRIBUTED_NODE
// bucket. Shard children (shard_count set — the __scan-shards launch
// path, provably not subbot children) never take the fallback.
// Preserves the input order (children arrive created_at asc) inside
// each bucket.
export function groupChildrenByNode(
  children: RunSummary[],
  wf: WireWorkflow | null,
): Map<string, RunSummary[]> {
  const subbots = subbotNodeIds(wf);
  const fallback =
    subbots.length === 1 ? subbots[0] ?? UNATTRIBUTED_NODE : UNATTRIBUTED_NODE;
  const byNode = new Map<string, RunSummary[]>();
  for (const child of children) {
    const key =
      child.parent_node_id ||
      (child.shard_count ? UNATTRIBUTED_NODE : fallback);
    const list = byNode.get(key);
    if (list) list.push(child);
    else byNode.set(key, [child]);
  }
  return byNode;
}

// childTabLabel derives the short tab label for a child run: the shard
// label when the spawner stamped one (fan_out_each item id), else the
// run's friendly name, else "<workflow> #<n>" from its list position.
export function childTabLabel(child: RunSummary, index: number): string {
  if (child.shard_label) return child.shard_label;
  if (child.name) return child.name;
  return `${child.workflow_name} #${index + 1}`;
}

// ChildStatusTone is the small palette bucket for sub-run status dots.
// `variant` mirrors runStatusMeta.STATUS_VARIANT (badge conventions)
// so dots and badges agree; `pulse` marks live work (running only).
interface ChildStatusTone {
  variant: "info" | "warning" | "success" | "danger" | "neutral";
  pulse: boolean;
}

function childStatusTone(status: RunStatus): ChildStatusTone {
  switch (status) {
    case "running":
      return { variant: "info", pulse: true };
    case "paused_waiting_human":
      return { variant: "warning", pulse: false };
    // Operator pause is info (no action required) — same distinction
    // runStatusMeta.STATUS_VARIANT draws.
    case "paused_operator":
      return { variant: "info", pulse: false };
    case "finished":
      return { variant: "success", pulse: false };
    case "failed":
    case "failed_resumable":
      return { variant: "danger", pulse: false };
    case "cancelled":
    case "queued":
    default:
      return { variant: "neutral", pulse: false };
  }
}

// Solid-fill dot classes per ChildStatusTone.variant. Full-strength
// status colors (not the -soft tints) — at 6px a tint is invisible.
const DOT_CLASS: Record<ChildStatusTone["variant"], string> = {
  info: "bg-info",
  warning: "bg-warning",
  success: "bg-success",
  danger: "bg-danger",
  neutral: "bg-fg-subtle",
};

// statusDotClass returns the classes for a small sub-run status dot —
// shared by the subbot frame's tab strip and IRNode's chip row so the
// surfaces stay color-identical.
export function statusDotClass(status: RunStatus): string {
  const tone = childStatusTone(status);
  return `${DOT_CLASS[tone.variant]}${tone.pulse ? " animate-pulse" : ""}`;
}

// isSettledRunStatus is true for statuses that won't change without an
// operator action (terminal + failed_resumable). Drives the children
// poll shutoff: once the parent AND every child are settled, nothing
// new can appear.
export function isSettledRunStatus(status: RunStatus): boolean {
  return (
    status === "finished" ||
    status === "failed" ||
    status === "failed_resumable" ||
    status === "cancelled"
  );
}

// Rank for the inline-frame default tab: live work first, then a
// human gate, then any other unsettled child, then settled history.
// Lower wins. Within a rank, later array position is the tie-break —
// both stores return children created_at asc, and groupChildrenByNode
// preserves that order. Do not compare created_at strings: Go's
// encoding/json RFC3339Nano trims fractional zeros, so ".12Z" > ".125Z"
// lexicographically and would pick the older child (issue #525).
function subbotDefaultRank(status: RunStatus): number {
  switch (status) {
    case "running":
      return 0;
    case "paused_waiting_human":
      return 1;
    case "queued":
    case "paused_operator":
      return 2;
    default:
      return 3;
  }
}

// Last auto-shown child for a frame, plus the children.length observed
// when it was shown. useSubbotLevel keeps one of these per frame so a
// 3s poll does not hop between already-visible siblings as their
// statuses flip. A new spawn (length grew) or a missing sticky id
// re-resolves — that is the #525 path.
export interface SubbotSelectionSticky {
  id: string;
  count: number;
}

// resolveSelectedSubbotChild picks which child run an inline subbot
// frame displays. An explicit user pick is kept while that child still
// exists. Else a still-present sticky id is kept while the list has not
// grown (no later child spawned). Else prefer running, then
// paused_waiting_human, then another unsettled child, then the latest
// child by array position. Never defaults to children[0] (oldest) — a
// historical failure would paint the current pipeline red (issue #525).
export function resolveSelectedSubbotChild(
  children: RunSummary[],
  picked?: string | null,
  sticky?: SubbotSelectionSticky | null,
): string | undefined {
  if (children.length === 0) return undefined;
  if (picked && children.some((c) => c.id === picked)) return picked;
  if (
    sticky &&
    children.some((c) => c.id === sticky.id) &&
    children.length <= sticky.count
  ) {
    return sticky.id;
  }
  let best = children[0]!;
  for (let i = 1; i < children.length; i++) {
    const c = children[i]!;
    if (subbotDefaultRank(c.status) <= subbotDefaultRank(best.status)) best = c;
  }
  return best.id;
}
