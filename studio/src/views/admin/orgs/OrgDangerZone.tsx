// Danger zone of the org drawer: schedule / cancel the soft-delete.
// Deletion schedules a purge after a 24h grace; the drawer stays open and
// flips to the "scheduled — cancel" state. reloadIdentity drops the
// now-blocked org from the switcher.

import { useState } from "react";

import { deleteOrg, restoreOrg, type OrgView } from "@/api/orgs";
import { Button } from "@/components/ui/Button";
import { Input } from "@/components/ui/Input";
import { formatDateTime } from "@/lib/format";

import { Field } from "./orgFields";

export function OrgDangerZone({
  org,
  busy,
  isActiveOrg,
  run,
  onAfterUpdate,
  reloadIdentity,
  onChanged,
}: {
  org: OrgView;
  busy: boolean;
  isActiveOrg: boolean;
  run: (fn: () => Promise<unknown>) => Promise<void>;
  onAfterUpdate: () => Promise<void>;
  reloadIdentity: () => Promise<void>;
  onChanged: (o: OrgView) => void;
}) {
  // Typed confirmation for the irreversible org deletion.
  const [confirmName, setConfirmName] = useState("");

  const scheduleDelete = () =>
    run(async () => {
      const updated = await deleteOrg(org.id);
      await onAfterUpdate();
      await reloadIdentity();
      setConfirmName("");
      onChanged(updated);
    });

  const cancelDelete = () =>
    run(async () => {
      const updated = await restoreOrg(org.id);
      await onAfterUpdate();
      await reloadIdentity();
      onChanged(updated);
    });

  return (
    <section className="space-y-3 mt-6 border-t border-danger/30 pt-4">
      <h4 className="font-medium text-danger">Danger zone</h4>
      {org.status === "pending_deletion" ? (
        <>
          <p className="text-xs text-fg-muted">
            Deletion scheduled
            {org.purge_after
              ? ` — will be permanently purged after ${formatDateTime(org.purge_after)}`
              : ""}
            . The organization is blocked until then. You can still cancel.
          </p>
          <Button variant="secondary" loading={busy} onClick={() => void cancelDelete()}>
            Cancel deletion
          </Button>
        </>
      ) : isActiveOrg ? (
        <p className="text-xs text-fg-muted">
          This is your active organization — switch to another org (top-left
          switcher) before you can delete it.
        </p>
      ) : (
        <>
          <p className="text-xs text-fg-muted">
            Schedules permanent deletion of <strong>{org.name}</strong>. The org is
            blocked immediately, then after a 24h grace a nightly job purges it,
            its teams, every membership, and all team data (runs, board, forge,
            secrets…). Cancellable until the grace elapses.
          </p>
          <Field label={`Type "${org.name}" to confirm`}>
            <Input
              value={confirmName}
              onChange={(e) => setConfirmName(e.target.value)}
              placeholder={org.name}
            />
          </Field>
          <Button
            variant="danger"
            loading={busy}
            disabled={confirmName.trim() !== org.name}
            onClick={() => void scheduleDelete()}
          >
            Schedule deletion (24h)
          </Button>
        </>
      )}
    </section>
  );
}
