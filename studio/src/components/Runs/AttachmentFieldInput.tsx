import { useEffect } from "react";

import type { AttachmentField, UploadLimits } from "@/api/types";
import { FileDropZone } from "@/components/shared/FileDropZone";
import { Button } from "@/components/ui/Button";
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
      {/* The zone is no control: the label inside FileDropZone owns the
          input, which is the one named thing that opens the picker. */}
      <FileDropZone
        name={`Upload ${field.name}`}
        accept={accept}
        onPick={acceptFile}
        disabled={disabled}
        busy={showProgress}
        invalid={Boolean(value?.error)}
      >
        {hasFile ? (
          <FilledPreview value={value!} field={field} />
        ) : (
          <EmptyHint isImage={isImage} description={field.description} />
        )}
      </FileDropZone>

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
