import { useEffect, useRef, useState } from "react";

import { uploadAttachment } from "@/api/runs";
import type { AttachmentField } from "@/api/types";
import { Button } from "@/components/ui/Button";
import { clickableRowProps } from "@/lib/a11y";
import { errorMessage } from "@/lib/errorHints";
import { formatBytes, validateAttachment } from "@/lib/attachmentValidation";
import { useServerInfoStore } from "@/store/serverInfo";

export interface GateFileValue {
  /** Staged upload id, present once the bytes reached the server. */
  uploadId: string;
  filename: string;
  size: number;
}

interface Props {
  /** Stable label used for the a11y action name. */
  label: string;
  value: GateFileValue | null;
  onChange: (next: GateFileValue | null) => void;
  /** Browser picker hint (e.g. "audio/*"). Advisory only. */
  accept?: string;
  /** Extra line under the drop zone (e.g. "MP3 or WAV"). */
  hint?: string;
  disabled?: boolean;
}

/**
 * Self-uploading file picker for a human gate.
 *
 * Distinct from Runs/AttachmentFieldInput, which is pure presentation
 * driven by LaunchView's orchestration: at a gate there is no launch
 * form to own the upload lifecycle, so this component performs the
 * upload itself and hands back only the staged id. That id is what the
 * answer carries — the bytes are already on the server by the time the
 * operator hits Submit, so submitting a 40 MB soundtrack does not turn
 * the resume request into a 40 MB request that can time out mid-flight.
 *
 * Validation is client-side courtesy only. The server re-sniffs the MIME
 * and re-checks every limit on upload; nothing here is a security
 * control.
 */
export default function GateFileInput({
  label,
  value,
  onChange,
  accept,
  hint,
  disabled,
}: Props) {
  const inputRef = useRef<HTMLInputElement | null>(null);
  const [dragging, setDragging] = useState(false);
  const [progress, setProgress] = useState<number | null>(null);
  const [error, setError] = useState<string | null>(null);
  const limits = useServerInfoStore((s) => s.info?.limits?.upload ?? null);

  // An upload that resolves after the form unmounted (gate answered in
  // another tab, run finished) must not setState on a dead component.
  const aliveRef = useRef(true);
  useEffect(() => {
    aliveRef.current = true;
    return () => {
      aliveRef.current = false;
    };
  }, []);

  const upload = async (file: File) => {
    setError(null);
    // Reuse the launch-path validator by describing this question as an
    // untyped attachment field: same size/MIME rules, one implementation.
    const field: AttachmentField = {
      name: label,
      type: "file",
      accept_mime: accept ? [accept] : undefined,
    };
    const check = validateAttachment(file, field, limits);
    if (!check.ok) {
      setError(check.error ?? "invalid file");
      return;
    }

    setProgress(0);
    // XHR fires progress far faster than React should re-render;
    // coalesce to whole percentage points.
    let lastPct = -1;
    try {
      const staged = await uploadAttachment(file, {
        declaredMime: file.type || undefined,
        onProgress: (loaded, total) => {
          const frac = total > 0 ? loaded / total : 0;
          const pct = Math.floor(frac * 100);
          if (pct === lastPct || !aliveRef.current) return;
          lastPct = pct;
          setProgress(frac);
        },
      });
      if (!aliveRef.current) return;
      setProgress(null);
      onChange({
        uploadId: staged.upload_id,
        filename: staged.original_filename || file.name,
        size: staged.size || file.size,
      });
    } catch (e) {
      if (!aliveRef.current) return;
      setProgress(null);
      setError(errorMessage(e));
    }
  };

  const uploading = progress !== null;
  const busy = disabled || uploading;

  return (
    <div className="space-y-1">
      <div
        {...clickableRowProps(() => inputRef.current?.click(), `Upload ${label}`)}
        aria-invalid={Boolean(error) || undefined}
        aria-busy={uploading || undefined}
        className={[
          "flex flex-col items-center justify-center gap-1.5 rounded-md border-dashed border p-3 text-center transition-colors cursor-pointer",
          dragging ? "border-accent bg-accent-soft" : "border-border-default",
          error ? "border-danger ring-1 ring-danger" : "",
          busy ? "opacity-60 pointer-events-none" : "hover:border-accent",
        ]
          .filter(Boolean)
          .join(" ")}
        onDragOver={(e) => {
          e.preventDefault();
          setDragging(true);
        }}
        onDragLeave={() => setDragging(false)}
        onDrop={(e) => {
          e.preventDefault();
          setDragging(false);
          const file = e.dataTransfer.files[0];
          if (file) void upload(file);
        }}
      >
        {value ? (
          <p className="text-xs truncate max-w-full">
            {value.filename}{" "}
            <span className="text-fg-subtle">· {formatBytes(value.size)}</span>
          </p>
        ) : (
          <>
            <p className="text-xs text-fg-muted">
              {uploading ? "Uploading…" : "Drop a file or click to browse"}
            </p>
            {hint && <p className="text-micro text-fg-subtle">{hint}</p>}
          </>
        )}
        <input
          ref={inputRef}
          type="file"
          accept={accept}
          className="sr-only"
          onChange={(e) => {
            const file = e.target.files?.[0];
            // Reset so re-picking the SAME file fires onChange again
            // (the operator replacing a file they just removed).
            e.target.value = "";
            if (file) void upload(file);
          }}
        />
      </div>

      {value && !uploading && (
        <div className="flex items-center justify-end">
          <Button
            type="button"
            variant="ghost"
            size="sm"
            onClick={(e) => {
              e.stopPropagation();
              onChange(null);
            }}
            disabled={disabled}
          >
            Remove
          </Button>
        </div>
      )}

      {uploading && (
        <div className="h-1 w-full bg-surface-2 rounded">
          <div
            className="h-1 bg-accent rounded transition-[width] duration-150"
            style={{ width: `${Math.round((progress ?? 0) * 100)}%` }}
          />
        </div>
      )}

      {error && (
        <p className="text-micro text-danger-fg" role="alert">
          {error}
        </p>
      )}
    </div>
  );
}
