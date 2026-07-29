import { useState } from "react";

import GateFileInput, {
  type GateFileValue,
} from "@/components/shared/GateFileInput";
import { Button } from "@/components/ui/Button";
import { formatBytes } from "@/lib/attachmentValidation";

interface Props {
  value: GateFileValue[];
  onChange: (next: GateFileValue[]) => void;
  disabled?: boolean;
}

/**
 * Ad-hoc attachments on a human gate — the always-available "here's a
 * diagram explaining what I mean" affordance.
 *
 * Deliberately requires NO DSL: a declared `file` field is the right
 * tool when the workflow KNOWS it needs a file (a soundtrack it cannot
 * proceed without), but most attachments are unplanned — the operator
 * is halfway through typing feedback and wants to point at a sketch.
 * Making that conditional on the bot author having anticipated it would
 * mean it is never there when it is actually needed.
 *
 * Collapsed to a single button until used, so gates that never see an
 * attachment pay no visual cost.
 */
export default function GateAttachments({ value, onChange, disabled }: Props) {
  const [picking, setPicking] = useState(false);

  const add = (next: GateFileValue | null) => {
    if (!next) return;
    onChange([...value, next]);
    setPicking(false);
  };

  const removeAt = (i: number) => {
    onChange(value.filter((_, idx) => idx !== i));
  };

  return (
    <div className="space-y-1.5">
      {value.length > 0 && (
        <ul className="space-y-1">
          {value.map((f, i) => (
            <li
              key={f.uploadId}
              className="flex items-center justify-between gap-2 text-micro text-fg-muted"
            >
              <span className="truncate">
                📎 {f.filename}
                <span className="text-fg-subtle"> · {formatBytes(f.size)}</span>
              </span>
              <Button
                type="button"
                variant="ghost"
                size="sm"
                disabled={disabled}
                onClick={() => removeAt(i)}
              >
                Remove
              </Button>
            </li>
          ))}
        </ul>
      )}

      {picking ? (
        <div className="space-y-1">
          <GateFileInput
            label="attachment"
            value={null}
            onChange={add}
            disabled={disabled}
          />
          <Button
            type="button"
            variant="ghost"
            size="sm"
            onClick={() => setPicking(false)}
            disabled={disabled}
          >
            Cancel
          </Button>
        </div>
      ) : (
        <button
          type="button"
          disabled={disabled}
          onClick={() => setPicking(true)}
          className="text-micro text-fg-subtle hover:text-fg-default disabled:opacity-50"
        >
          📎 {value.length > 0 ? "Attach another file" : "Attach a file"}
        </button>
      )}
    </div>
  );
}
