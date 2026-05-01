import { useCallback } from "react";

import type { CompactionBlock } from "@/api/types";
import { NumberField } from "./FormField";

interface Props {
  value: CompactionBlock | undefined;
  onChange: (block: CompactionBlock | undefined) => void;
}

/** Workflow- and node-level session compaction settings.
 *
 *  Both fields use a "0/nil = inherit" convention: omit the field to
 *  fall through to the workflow default (or the engine baseline). The
 *  threshold is a ratio of the model's context window (0 < t <= 1);
 *  preserve_recent is the count of trailing messages kept verbatim
 *  during compaction. */
export default function CompactionFields({ value, onChange }: Props) {
  const set = useCallback(
    (patch: Partial<CompactionBlock>) => {
      const next: CompactionBlock = { ...(value ?? {}), ...patch };
      // Drop empty blocks so the JSON omits `compaction` entirely
      // rather than emitting `{}` (which the parser may treat as
      // "explicit empty" and refuse to inherit).
      if (next.threshold === undefined && next.preserve_recent === undefined) {
        onChange(undefined);
      } else {
        onChange(next);
      }
    },
    [value, onChange],
  );

  return (
    <details className="border-t border-border-default pt-2 mt-2">
      <summary className="cursor-pointer text-xs text-fg-subtle font-semibold mb-1">
        Compaction <span className="text-fg-subtle">?</span>
      </summary>
      <div className="pl-2">
        <NumberField
          label="Threshold (ratio of context window)"
          value={value?.threshold}
          onChange={(v) => set({ threshold: v === undefined || v <= 0 ? undefined : v })}
          min={0}
          help="Ratio of the model's context window at which compaction triggers. e.g. 0.85 = compact at 85% full. Empty = inherit."
        />
        <NumberField
          label="Preserve recent"
          value={value?.preserve_recent}
          onChange={(v) => set({ preserve_recent: v && v >= 1 ? Math.trunc(v) : undefined })}
          min={1}
          help="Number of trailing messages to keep verbatim during compaction. Empty = inherit."
        />
      </div>
    </details>
  );
}
