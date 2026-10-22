import { useEffect, useRef, useState } from "react";

import type { AttachmentField, UploadLimits } from "@/api/types";
import { Button } from "@/components/ui/Button";
import { clickableRowProps } from "@/lib/a11y";
import { formatBytes, validateAttachment } from "@/lib/attachmentValidation";

export interface AttachmentValue {
  file: File;
  uploadId?: string;
  /** 0..1 progress while uploading; undefined when idle. */
  progress?: number;
  /** Last validation/upload error to display inline. */
  error?: string;
  /** ObjectURL for image preview thumbnails. Caller must revoke. */
  previewUrl?: string;
}

interface Props {
  field: AttachmentField;
  value: AttachmentValue | null;
  onChange: (next: AttachmentValue | null) => void;
  serverLimits: UploadLimits | null;
  /** When set, the input is disabled (e.g. submit in progress). */
  disabled?: boolean;
}

/**
 * Per-attachment file input: drag-and-drop zone, click-to-pick fallback,
 * image preview, progress bar while uploading, and inline error display.
 *
 * Pure presentation: validation happens via lib/attachmentValidation,
 * the actual upload is orchestrated by LaunchView so multiple
 * attachments can share a single AbortController.
 */
export default function AttachmentFieldInput({
  field,
  value,
  onChange,
  serverLimits,
  disabled,
}: Props) {
  const inputRef = useRef<HTMLInputElement | null>(null);
  const [dragging, setDragging] = useState(false);

  // Revoke the previous ObjectURL on unmount or when value changes.
  useEffect(() => {
    return () => {
      if (value?.previewUrl) URL.revokeObjectURL(value.previewUrl);
    };
  }, [value?.previewUrl]);

  const accept =
    field.accept_mime?.join(",") ?? (field.type === "image" ? "image/*" : undefined);

  const acceptFile = (file: File | null | undefined) => {
    if (!file) return;
    const result = validateAttachment(file, field, serverLimits);
    if (!result.ok) {
      onChange({ file, error: result.error });
      return;
    }
    let previewUrl: string | undefined;
    if (field.type === "image") {
      previewUrl = URL.createObjectURL(file);
    }
    onChange({ file, previewUrl });
  };

  const isImage = field.type === "image";
  const hasFile = Boolean(value?.file);
  const showProgress = typeof value?.progress === "number";

  return (
    <div className="space-y-1">
      <div
        {...clickableRowProps(() => inputRef.current?.click(), `Upload ${field.name}`)}
        aria-invalid={Boolean(value?.error) || undefined}
        aria-busy={showProgress || undefined}
        aria-disabled={disabled || undefined}
        data-disabled={disabled || undefined}
        className={[
          "flex flex-col items-center justify-center gap-1.5 rounded-md border-dashed border p-3 text-center transition-colors cursor-pointer data-[disabled]:opacity-60",
          dragging ? "border-accent bg-accent-soft" : "border-border-default",
          value?.error ? "border-danger ring-1 ring-danger" : "",
          disabled ? "pointer-events-none" : "hover:border-accent",
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
          if (file) acceptFile(file);
        }}
      >
        {hasFile ? (
          <FilledPreview value={value!} field={field} />
        ) : (
          <EmptyHint isImage={isImage} description={field.description} />
        )}
        <input
          ref={inputRef}
          type="file"
          accept={accept}
          className="sr-only"
          aria-label={`Upload ${field.name}`}
          onChange={(e) => acceptFile(e.target.files?.[0] ?? null)}
        />
      </div>

      {hasFile && !showProgress && (
        <div className="flex items-center justify-between text-micro text-fg-muted">
          <span className="truncate">
            {value!.file.name} · {formatBytes(value!.file.size)}
            {value!.uploadId ? " · uploaded" : ""}
          </span>
          <Button
            type="button"
            variant="ghost"
            size="sm"
            onClick={(e) => {
              e.stopPropagation();
              if (value?.previewUrl) URL.revokeObjectURL(value.previewUrl);
              onChange(null);
            }}
            disabled={disabled}
          >
            Remove
          </Button>
        </div>
      )}

      {showProgress && (
        <div className="h-1 w-full bg-surface-2 rounded">
          <div
            className="h-1 bg-accent rounded transition-[width] duration-150"
            style={{ width: `${Math.round((value!.progress ?? 0) * 100)}%` }}
          />
        </div>
      )}

      {value?.error && (
        <p className="text-micro text-danger-fg" role="alert">
          {value.error}
        </p>
      )}
    </div>
  );
}

function EmptyHint({ isImage, description }: { isImage: boolean; description?: string }) {
  return (
    <>
      <p className="text-xs text-fg-muted">
        {isImage ? "Drop an image or click to browse" : "Drop a file or click to browse"}
      </p>
      {description && <p className="text-micro text-fg-subtle">{description}</p>}
    </>
  );
}

function FilledPreview({ value, field }: { value: AttachmentValue; field: AttachmentField }) {
  if (field.type === "image" && value.previewUrl) {
    return (
      <img
        src={value.previewUrl}
        alt={value.file.name}
        className="max-h-32 max-w-full object-contain"
      />
    );
  }
  return (
    <p className="text-xs">
      {value.file.name} ·{" "}
      <span className="font-mono text-fg-subtle">{value.file.type || "unknown"}</span>
    </p>
  );
}
