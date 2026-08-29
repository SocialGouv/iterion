import { useState } from "react";

import { Button } from "@/components/ui/Button";
import { Textarea } from "@/components/ui/Textarea";

interface Props {
  hasTextField: boolean;
  busy: boolean;
  onSubmit: (approved: boolean, text?: string) => Promise<void>;
}

/** Approval input shared by both assistant surfaces.
 *
 * Approval-only turns expose an immediate reject action. Hybrid turns collect
 * revision text before sending false, while both variants always send a real
 * boolean through useAssistantComposer.
 */
export default function AssistantApprovalComposer({
  hasTextField,
  busy,
  onSubmit,
}: Props) {
  const [revisionOpen, setRevisionOpen] = useState(false);
  const [draft, setDraft] = useState("");

  const submit = async (approved: boolean, text?: string) => {
    await onSubmit(approved, text);
    setDraft("");
    setRevisionOpen(false);
  };

  return (
    <div className="space-y-2 px-3 py-2" aria-label="Approval response">
      <div className="flex flex-wrap gap-2">
        <Button
          variant="primary"
          size="sm"
          loading={busy}
          disabled={busy}
          onClick={() => void submit(true).catch(() => {})}
        >
          {busy ? "Sending…" : "Approve"}
        </Button>
        <Button
          variant="secondary"
          size="sm"
          disabled={busy}
          onClick={() => {
            if (hasTextField) setRevisionOpen((open) => !open);
            else void submit(false).catch(() => {});
          }}
        >
          {hasTextField ? "Request revision" : "Reject"}
        </Button>
      </div>
      {hasTextField && revisionOpen && (
        <div className="flex items-end gap-2">
          <Textarea
            value={draft}
            onChange={(event) => setDraft(event.target.value)}
            placeholder="What should be revised?"
            rows={3}
            disabled={busy}
            className="flex-1"
          />
          <Button
            variant="primary"
            size="sm"
            loading={busy}
            disabled={busy || draft.trim() === ""}
            onClick={() => void submit(false, draft).catch(() => {})}
          >
            {busy ? "Sending…" : "Send"}
          </Button>
        </div>
      )}
    </div>
  );
}
