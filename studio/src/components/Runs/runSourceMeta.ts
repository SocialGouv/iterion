import {
  ClockIcon,
  LightningBoltIcon,
  PersonIcon,
  RocketIcon,
  Share1Icon,
  ViewGridIcon,
} from "@radix-ui/react-icons";

import type { BadgeVariant } from "@/components/ui/Badge";
import type { RunSourceKind, RunSummary } from "@/api/runs";

// Radix icons are forward-ref components; reuse one of them as the
// canonical type so the SourceMeta map type-checks without inventing a
// looser ComponentType<SVGAttributes> that the radix types reject
// (children: never).
type RadixIcon = typeof PersonIcon;

// Concrete order the chip row and group headers walk. Trigger sources
// (manual / webhook / schedule / dispatcher) come first — they're how an
// operator thinks about "where did this run come from"; structural ones
// (fork / shard) follow. The "all" pseudo-value is rendered separately.
export const SOURCE_KIND_ORDER: ReadonlyArray<RunSourceKind> = [
  "manual",
  "webhook",
  "schedule",
  "dispatcher",
  "fork",
  "shard",
];

// SourceMeta drives the source badge + filter chips + group headers.
// Variant maps to Badge's existing palette so the new chip slots
// straight into the run rows without inventing a new style.
export interface SourceMeta {
  label: string;
  // Short description for the badge title / tooltip — supplements the
  // glyph when an operator hovers a row.
  description: string;
  Icon: RadixIcon;
  variant: BadgeVariant;
}

export const SOURCE_META: Record<RunSourceKind, SourceMeta> = {
  // Plain operator launch (CLI, studio Run, cloud REST). The default;
  // also covers legacy runs that predate the source_kind field.
  manual: {
    label: "Manual",
    description: "Launched by an operator (CLI, studio, or REST).",
    Icon: PersonIcon,
    variant: "neutral",
  },
  // Inbound HTTP webhook (forge events, GitLab/GitHub, etc.) — the
  // service event spine in cloud mode.
  webhook: {
    label: "Webhook",
    description: "Triggered by an inbound webhook event.",
    Icon: LightningBoltIcon,
    variant: "accent",
  },
  // Recurring cron schedule — host-crontab, trigger spine, or the cloud
  // ticker. Stamped with the schedule identity the overlap gate meters on.
  schedule: {
    label: "Schedule",
    description: "Fired by a recurring cron schedule.",
    Icon: ClockIcon,
    // Shares dispatcher's palette on purpose — both are autonomous machine
    // triggers; the clock glyph is what tells them apart.
    variant: "info",
  },
  // Long-running dispatcher polling an issue tracker (native kanban,
  // GitHub Issues, Forgejo). The most autonomous trigger source.
  dispatcher: {
    label: "Dispatcher",
    description: "Spawned by a polling dispatcher per tracker issue.",
    Icon: RocketIcon,
    variant: "info",
  },
  // Resumed-from-a-prior-turn child run (POST /runs/:id/fork). Share1Icon
  // is the closest radix has to a branching glyph.
  fork: {
    label: "Fork",
    description: "Forked from a prior LLM turn of another run.",
    Icon: Share1Icon,
    variant: "warning",
  },
  // Internal shard (parent_run_id set). Rare in surface, but worth
  // distinguishing in the list so an operator can tell a child run
  // apart from its parent.
  shard: {
    label: "Shard",
    description: "Internal shard of a parent run.",
    Icon: ViewGridIcon,
    variant: "neutral",
  },
};

// normalizeSourceKind collapses the wire's optional/empty value to the
// "manual" default so every consumer can pattern-match without
// repeating the legacy guard. Unknown future strings fall back to
// "manual" too — a stale frontend should still render the row, just
// without a tailored icon.
export function normalizeSourceKind(raw: string | undefined | null): RunSourceKind {
  switch (raw) {
    case "webhook":
    case "schedule":
    case "dispatcher":
    case "fork":
    case "shard":
    case "manual":
      return raw;
    default:
      return "manual";
  }
}

// runSourceKind is the canonical accessor: hands every UI surface the
// same normalised value off the RunSummary, regardless of which field
// the backend populated.
export function runSourceKind(run: Pick<RunSummary, "source_kind">): RunSourceKind {
  return normalizeSourceKind(run.source_kind);
}

// metaForSource is the SourceMeta lookup that mirrors STATUS_VARIANT +
// labelForStatus in spirit — every consumer asks for "the metadata I
// should render", never reaches into SOURCE_META directly.
export function metaForSource(kind: RunSourceKind): SourceMeta {
  return SOURCE_META[kind] ?? SOURCE_META.manual;
}
