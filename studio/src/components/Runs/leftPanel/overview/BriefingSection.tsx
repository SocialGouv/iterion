import { CopyButton, LinkifiedText } from "@/components/ui";
import type { RunHeader } from "@/api/runs";

import { Section } from "../InfoPrimitives";

interface BriefingSectionProps {
  run: RunHeader;
}

// BriefingSection renders what the run was ASKED TO DO: its headline
// briefing (the improvement axis / brief / prompt) in full, then the
// remaining launch inputs. A long axis starts collapsed so the Budget +
// Progress sections above it stay visible without scrolling.
export function BriefingSection({ run }: BriefingSectionProps) {
  const inputs = run.inputs ?? {};
  const { key: axisKey, value: axis } = readAxis(inputs);
  const otherInputEntries = Object.entries(inputs).filter(
    ([k]) => k !== axisKey,
  );

  if (axis === null && otherInputEntries.length === 0) return null;

  return (
    <>
      {axis !== null && (
        <Section
          title={axisSectionTitle(axisKey)}
          collapsible
          defaultOpen={axis.length < 240}
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
            <LinkifiedText text={axis} />
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
    </>
  );
}

// Keys we recognise as "the operator's headline briefing" — checked in
// this order. `improvement_prompt` is what every improve-loop bot uses;
// `feature_prompt` is feature_dev's; `brief` and `prompt` are common
// alternates on ad-hoc bots.
const AXIS_KEYS = [
  "improvement_prompt",
  "feature_prompt",
  "brief",
  "prompt",
] as const;
// Threshold above which a value escapes a single-line Row into its own
// scrollable <pre>. Multi-line values (any \n) also break out.
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

// InputRow renders one non-axis launch input. Short values fit a standard
// 80px-label row; long values (multi-line prompts, glob lists, JSON
// blobs) escape into a scrollable pre so the operator can actually read
// them.
function InputRow({ label, value }: InputRowProps) {
  const asString = stringifyValue(value);
  const isEmpty = asString.length === 0;
  const isLong =
    !isEmpty &&
    (asString.length > LONG_INPUT_THRESHOLD || asString.includes("\n"));

  if (isEmpty) {
    return (
      <div className="grid grid-cols-[80px_1fr] gap-2 text-micro items-start">
        <span className="text-fg-subtle break-all" title={label}>
          {label}
        </span>
        <span className="text-fg-subtle italic">(empty)</span>
      </div>
    );
  }

  if (!isLong) {
    return (
      <div className="grid grid-cols-[80px_1fr_auto] gap-2 text-micro items-start">
        <span className="text-fg-subtle break-all" title={label}>
          {label}
        </span>
        <code
          className="font-mono text-caption text-fg-default break-all min-w-0"
          title={asString}
        >
          <LinkifiedText text={asString} />
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
      <div className="flex items-center justify-between gap-2">
        <span className="min-w-0 break-all text-micro text-fg-subtle" title={label}>
          {label}
        </span>
        <CopyButton
          value={asString}
          variant="icon"
          label={`Copy ${label}`}
          copiedLabel="Copied"
        />
      </div>
      <pre className="m-0 max-h-40 overflow-auto whitespace-pre-wrap break-words rounded border border-border-subtle bg-surface-2 px-2 py-1.5 font-mono text-caption text-fg-default">
        <LinkifiedText text={asString} />
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
