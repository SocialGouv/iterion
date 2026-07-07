import { type ReactNode } from "react";

import { CopyButton, StatusBadge } from "@/components/ui";
import type { RunHeader, RunModelOverride } from "@/api/runs";

import { Row, Section } from "./InfoPrimitives";

interface OverviewPanelProps {
  run: RunHeader | null;
}

// OverviewPanel is the run-console's "config first" surface. It leads
// with the run's IMPROVEMENT AXIS (its briefing prompt) rendered in
// full, then the remaining launch inputs, then a compact "Launched
// with" summary of the run's persisted configuration facts.
//
// The point is to make what the run was ASKED TO DO instantly
// readable — separate from its OUTPUT (diffs, commits, runtime timing)
// which live on the peer Files/Commits tabs and in the Info tab. Before
// this panel, the axis lived in InfoPanel's Inputs section truncated at
// 80 chars — unreadable for a multi-sentence briefing.
export default function OverviewPanel({ run }: OverviewPanelProps) {
  if (!run) {
    return (
      <div className="flex flex-col min-h-0 min-w-0 flex-1 w-full items-center justify-center px-3 py-8 text-center text-xs text-fg-subtle">
        Loading…
      </div>
    );
  }

  const inputs = run.inputs ?? {};
  const { key: axisKey, value: axis } = readAxis(inputs);
  const otherInputEntries = Object.entries(inputs).filter(
    ([k]) => k !== axisKey,
  );
  const launchedWith = collectLaunchedWith(run);
  const hasBrief =
    axis !== null || otherInputEntries.length > 0 || launchedWith.length > 0;

  return (
    <div className="flex flex-col min-h-0 min-w-0 flex-1 w-full overflow-y-auto">
      <div className="px-3 py-2 space-y-3">
        {/* Identity/status at the top so operators can orient without
            switching to the Info tab. Kept minimal — full metadata
            (timing, worktree paths, error) stays on Info. */}
        <Section title="Run">
          <Row label="Status">
            <StatusBadge status={run.status} />
          </Row>
          <Row label="Name">
            <span className="truncate">{run.name || run.workflow_name}</span>
          </Row>
        </Section>

        {axis !== null && (
          <Section
            title={axisSectionTitle(axisKey)}
            headerRight={
              <CopyButton
                value={axis}
                variant="icon"
                label="Copy axis"
                copiedLabel="axis copied"
              />
            }
          >
            <pre className="m-0 max-h-64 overflow-auto whitespace-pre-wrap break-words rounded border border-border-subtle bg-surface-2 px-2 py-1.5 font-mono text-caption text-fg-default">
              {axis}
            </pre>
          </Section>
        )}

        {otherInputEntries.length > 0 && (
          <Section title="Inputs">
            {otherInputEntries.map(([k, v]) => (
              <InputRow key={k} label={k} value={v} />
            ))}
          </Section>
        )}

        {launchedWith.length > 0 && (
          <Section title="Launched with">
            {launchedWith.map((f) => (
              <Row key={f.label} label={f.label}>
                {f.render}
              </Row>
            ))}
          </Section>
        )}

        {!hasBrief && (
          <div className="rounded border border-border-subtle bg-surface-1 px-3 py-4 text-caption text-fg-subtle">
            No launch inputs or configuration recorded for this run.
          </div>
        )}
      </div>
    </div>
  );
}

// Keys we recognise as "the operator's headline briefing" — checked
// in this order. `improvement_prompt` is what every improve-loop bot
// (whole-improve-loop, branch-improve-loop, ...) uses; `brief` and
// `prompt` are common alternates on ad-hoc bots.
const AXIS_KEYS = ["improvement_prompt", "brief", "prompt"] as const;
// Threshold above which we escape a value out of a single-line Row and
// into its own scrollable <pre> block. Multi-line values (any \n) also
// break out regardless of length.
const LONG_INPUT_THRESHOLD = 200;

function readAxis(inputs: Record<string, unknown>): {
  key: string | null;
  value: string | null;
} {
  for (const k of AXIS_KEYS) {
    const v = inputs[k];
    if (typeof v === "string" && v.trim().length > 0) {
      return { key: k, value: v };
    }
  }
  return { key: null, value: null };
}

function axisSectionTitle(key: string | null): string {
  if (key === "brief") return "Brief";
  if (key === "prompt") return "Prompt";
  return "Improvement axis";
}

interface InputRowProps {
  label: string;
  value: unknown;
}

// InputRow renders one non-axis launch input. Short values fit in a
// standard 80px-label row; long values (multi-line prompts, glob lists,
// big JSON blobs) escape into a scrollable pre so the operator can
// actually READ them — the old InfoPanel truncated at 80 chars, which
// left multi-sentence prompts and long glob lists unreadable without a
// hover tooltip.
function InputRow({ label, value }: InputRowProps) {
  const asString = stringifyValue(value);
  const isEmpty = asString.length === 0;
  const isLong =
    !isEmpty &&
    (asString.length > LONG_INPUT_THRESHOLD || asString.includes("\n"));

  if (isEmpty) {
    return (
      <div className="grid grid-cols-[80px_1fr] gap-2 text-micro">
        <span className="text-fg-subtle truncate">{label}</span>
        <span className="text-fg-subtle italic">(empty)</span>
      </div>
    );
  }

  if (!isLong) {
    return (
      <div className="grid grid-cols-[80px_1fr_auto] gap-2 text-micro items-start">
        <span className="text-fg-subtle truncate">{label}</span>
        <code
          className="font-mono text-caption text-fg-default break-all min-w-0"
          title={asString}
        >
          {asString}
        </code>
        <CopyButton
          value={asString}
          variant="icon"
          label={`Copy ${label}`}
          copiedLabel="Copied"
        />
      </div>
    );
  }

  return (
    <div className="space-y-1">
      <div className="flex items-center justify-between">
        <span className="text-micro text-fg-subtle">{label}</span>
        <CopyButton
          value={asString}
          variant="icon"
          label={`Copy ${label}`}
          copiedLabel="Copied"
        />
      </div>
      <pre className="m-0 max-h-40 overflow-auto whitespace-pre-wrap break-words rounded border border-border-subtle bg-surface-2 px-2 py-1.5 font-mono text-caption text-fg-default">
        {asString}
      </pre>
    </div>
  );
}

function stringifyValue(v: unknown): string {
  if (v === null || v === undefined) return "";
  if (typeof v === "string") return v;
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  try {
    return JSON.stringify(v, null, 2);
  } catch {
    return String(v);
  }
}

interface LaunchedWithField {
  label: string;
  render: ReactNode;
}

// collectLaunchedWith produces the compact "how was this run
// configured?" summary. We render only fields ACTUALLY carried on the
// RunHeader wire (see pkg/runview/snapshot.go RunHeader). model_overrides
// IS persisted (so the Overview shows what a run launched with);
// review_mode is resolved into inputs (shown in the Inputs section);
// other options like compress ride CreateRunRequest but aren't on the
// header, so we don't fabricate rows for them.
function collectLaunchedWith(run: RunHeader): LaunchedWithField[] {
  const fields: LaunchedWithField[] = [];

  if (run.bundle_display_name || run.bundle_name) {
    fields.push({
      label: "Bot",
      render: (
        <span className="truncate">
          {run.bundle_display_name || run.bundle_name}
        </span>
      ),
    });
  }

  if (run.permission_mode && run.permission_mode !== "off") {
    fields.push({
      label: "Permission",
      render: <span>{run.permission_mode}</span>,
    });
  }

  if (run.model_overrides && run.model_overrides.length > 0) {
    fields.push({
      label: run.model_overrides.length > 1 ? "Models" : "Model",
      render: (
        <span className="flex flex-col gap-0.5">
          {run.model_overrides.map((o, i) => (
            <span key={i} className="font-mono text-caption break-words">
              {formatModelOverride(o)}
            </span>
          ))}
        </span>
      ),
    });
  }

  fields.push({
    label: "Worktree",
    render: (
      <span>
        {run.worktree
          ? "auto (fresh git worktree)"
          : "off (project working dir)"}
      </span>
    ),
  });

  if (run.worktree) {
    fields.push({
      label: "Merge",
      render: (
        <span>
          {run.merge_strategy || "squash"}
          {" · "}
          {run.auto_merge ? "auto-merge on" : "auto-merge off"}
        </span>
      ),
    });
  }

  if (run.source?.kind) {
    fields.push({
      label: "Source",
      render: <span>{formatSource(run.source)}</span>,
    });
  }

  return fields;
}

// formatModelOverride renders one override rule as
// "<selector> → <pins>", where pins is the non-empty subset of
// model / backend / provider (e.g. "agent → claude-sonnet-5" or
// "reviewer_* → gpt-5.5 · claw").
function formatModelOverride(o: RunModelOverride): string {
  const pins = [o.model, o.backend, o.provider].filter(Boolean).join(" · ");
  return pins ? `${o.selector} → ${pins}` : o.selector;
}

function formatSource(source: NonNullable<RunHeader["source"]>): string {
  const kind = source.kind ?? "manual";
  if (source.issue_identifier) return `${kind} · ${source.issue_identifier}`;
  if (source.issue_title) return `${kind} · ${source.issue_title}`;
  return kind;
}
