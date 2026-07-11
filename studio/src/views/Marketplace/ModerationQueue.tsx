import { useState } from "react";

import type { MarketplaceEntry } from "@/api/marketplace";
import { Button } from "@/components/ui/Button";
import { Dialog } from "@/components/ui/Dialog";
import { Textarea } from "@/components/ui/Textarea";

interface Props {
  entries: MarketplaceEntry[];
  onApprove: (slug: string) => Promise<void>;
  onReject: (slug: string, reason: string) => Promise<void>;
}

/** ModerationQueue lists entries awaiting review (cloud, admin-only).
 *  Each row offers Approve / Reject; reject opens a dialog asking for an
 *  optional reason surfaced back to the submitter. Hidden by the parent
 *  when the queue is empty (or the caller isn't an admin → the fetch
 *  403s). */
export function ModerationQueue({ entries, onApprove, onReject }: Props) {
  const [busy, setBusy] = useState<string | null>(null);
  // The entry a reject is being composed for; null = dialog closed.
  const [rejecting, setRejecting] = useState<MarketplaceEntry | null>(null);
  const [reason, setReason] = useState("");

  const openReject = (e: MarketplaceEntry) => {
    setReason("");
    setRejecting(e);
  };

  const confirmReject = async () => {
    if (!rejecting) return;
    const slug = rejecting.slug;
    setBusy(slug);
    try {
      await onReject(slug, reason.trim());
      setRejecting(null);
    } finally {
      setBusy(null);
    }
  };

  const approve = async (slug: string) => {
    setBusy(slug);
    try {
      await onApprove(slug);
    } finally {
      setBusy(null);
    }
  };

  return (
    <section className="flex flex-col gap-2 rounded border border-warning/40 bg-warning-soft/30 p-3">
      <h2 className="text-xs font-medium text-fg-default">
        Pending review ({entries.length})
      </h2>
      <ul className="flex flex-col gap-1.5">
        {entries.map((e) => (
          <li
            key={e.slug}
            className="flex items-center justify-between gap-2 rounded border border-border-default bg-surface-1 px-2 py-1.5"
          >
            <div className="min-w-0">
              <div className="flex items-baseline gap-1.5">
                <span className="truncate text-sm font-medium text-fg-default">
                  {e.display_name?.trim() || e.name}
                </span>
                {e.scope && (
                  <span className="shrink-0 rounded bg-surface-2 px-1.5 py-0.5 text-caption text-fg-muted">
                    {e.scope}
                  </span>
                )}
              </div>
              <p className="truncate font-mono text-caption text-fg-subtle">
                {e.repo_url}
                {e.ref ? `#${e.ref}` : ""}
              </p>
            </div>
            <div className="flex shrink-0 items-center gap-1.5">
              <Button
                variant="success"
                size="sm"
                onClick={() => void approve(e.slug)}
                disabled={busy === e.slug}
              >
                Approve
              </Button>
              <Button
                variant="danger"
                size="sm"
                onClick={() => openReject(e)}
                disabled={busy === e.slug}
              >
                Reject
              </Button>
            </div>
          </li>
        ))}
      </ul>

      <Dialog
        open={rejecting !== null}
        onOpenChange={(open) => {
          // Keep the dialog up while the reject request is in flight.
          if (!open && busy === null) setRejecting(null);
        }}
        title={`Reject "${rejecting?.display_name?.trim() || rejecting?.name || ""}"`}
        description="The reason is surfaced back to the submitter."
        footer={
          <>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => setRejecting(null)}
              disabled={busy !== null}
            >
              Cancel
            </Button>
            <Button
              variant="danger"
              size="sm"
              onClick={() => void confirmReject()}
              disabled={busy !== null}
              loading={busy !== null}
            >
              {busy !== null ? "Rejecting…" : "Reject"}
            </Button>
          </>
        }
      >
        <label className="flex flex-col gap-1">
          <span className="text-caption uppercase tracking-wide text-fg-subtle">
            Reason (optional)
          </span>
          <Textarea
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            rows={3}
            placeholder="e.g. manifest missing a description, repo not reachable…"
            disabled={busy !== null}
          />
        </label>
      </Dialog>
    </section>
  );
}
