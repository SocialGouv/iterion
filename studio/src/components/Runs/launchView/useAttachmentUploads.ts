// Extracted from LaunchView.tsx to keep that file focused.
// useAttachmentUploads owns the attachment form state and the
// upload-on-select flow feeding staged upload ids into the launch call.

import { useState } from "react";

import { uploadAttachment } from "@/api/runs";
import type { AttachmentField } from "@/api/types";
import { errorMessage } from "@/lib/errorHints";

import { type AttachmentValue } from "../AttachmentFieldInput";

export function useAttachmentUploads() {
  const [attachments, setAttachments] = useState<
    Record<string, AttachmentValue | null>
  >({});

  // Auto-upload as soon as a file is selected. The upload runs in the
  // background and the launch button stays disabled until every entry
  // either has an uploadId or is optional and absent.
  const handleAttachmentChange = async (
    field: AttachmentField,
    next: AttachmentValue | null,
  ) => {
    setAttachments((prev) => ({ ...prev, [field.name]: next }));
    if (!next || next.error || next.uploadId) return;
    // Kick off the upload.
    setAttachments((prev) => ({
      ...prev,
      [field.name]: { ...next, progress: 0 },
    }));
    // Throttle progress updates to once per percentage step. XHR can
    // emit progress 100+ times per second on a fast pipe; coalescing
    // here keeps the re-render budget bounded to ~100 per attachment.
    let lastPct = -1;
    try {
      const staged = await uploadAttachment(next.file, {
        declaredMime: next.file.type || undefined,
        onProgress: (loaded, total) => {
          const frac = total > 0 ? loaded / total : 0;
          const pct = Math.floor(frac * 100);
          if (pct === lastPct) return;
          lastPct = pct;
          setAttachments((prev) => {
            const cur = prev[field.name];
            if (!cur || cur.file !== next.file) return prev;
            return {
              ...prev,
              [field.name]: { ...cur, progress: frac },
            };
          });
        },
      });
      setAttachments((prev) => {
        const cur = prev[field.name];
        if (!cur || cur.file !== next.file) return prev;
        return {
          ...prev,
          [field.name]: {
            ...cur,
            uploadId: staged.upload_id,
            progress: undefined,
          },
        };
      });
    } catch (err) {
      setAttachments((prev) => {
        const cur = prev[field.name];
        if (!cur || cur.file !== next.file) return prev;
        return {
          ...prev,
          [field.name]: {
            ...cur,
            error: errorMessage(err),
            progress: undefined,
          },
        };
      });
    }
  };

  return { attachments, handleAttachmentChange };
}
