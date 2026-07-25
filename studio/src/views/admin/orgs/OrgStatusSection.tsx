// Org status editor: active / suspended / read-only, with an audit-log
// reason and a two-step inline confirm for the disruptive statuses.

import { useState } from "react";

import { setOrgStatus, type OrgView } from "@/api/orgs";
import { Button } from "@/components/ui/Button";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";

import { Field } from "./orgFields";
import { orgStatusMeta } from "./orgStatusMeta";

export function OrgStatusSection({
  org,
  busy,
  run,
  onAfterUpdate,
}: {
  org: OrgView;
  busy: boolean;
  run: (fn: () => Promise<unknown>) => Promise<void>;
  onAfterUpdate: () => Promise<void>;
}) {
  const [statusDraft, setStatusDraft] = useState<string>(org.status);
  const [reason, setReason] = useState("");
  // Two-step confirm for the disruptive (suspend / read_only) statuses.
  // Inline (not useConfirm) because this lives inside a Radix Dialog,
  // where a body-portaled ConfirmDialog reads as an outside-click and
  // dismisses the parent — see ProjectSwitcher for the same precedent.
  const [confirmStatus, setConfirmStatus] = useState(false);

  const saveStatus = () =>
    run(async () => {
      await setOrgStatus(org.id, statusDraft, reason.trim() || undefined);
      await onAfterUpdate();
    });

  const disruptive = statusDraft === "suspended" || statusDraft === "read_only";

  return (
    <section className="space-y-3">
      <h4 className="font-medium">Status</h4>
      <div className="grid grid-cols-1 sm:grid-cols-2 gap-2">
        <Field label="Status">
          <Select
            value={statusDraft}
            onChange={(e) => {
              setStatusDraft(e.target.value);
              setConfirmStatus(false);
            }}
          >
            <option value="active">{orgStatusMeta("active").label}</option>
            <option value="suspended">{orgStatusMeta("suspended").label}</option>
            <option value="read_only">{orgStatusMeta("read_only").label}</option>
          </Select>
          <p className="text-xs text-fg-muted">
            {statusDraft === "suspended"
              ? "Blocks all new run launches org-wide (studio, API, inbound webhooks). Existing data stays readable."
              : statusDraft === "read_only"
                ? "Members can still sign in and read runs and boards, but no new runs can launch (studio, API, inbound webhooks)."
                : "Normal operation — members can launch runs."}
          </p>
        </Field>
        <Field label="Reason (audit log)">
          <Input
            value={reason}
            onChange={(e) => setReason(e.target.value)}
            placeholder="optional"
          />
        </Field>
      </div>
      {disruptive && confirmStatus ? (
        <div className="flex flex-wrap items-center gap-2">
          <span className="text-xs text-danger">
            Applies <strong>{orgStatusMeta(statusDraft).label}</strong> to the whole org
            immediately.
          </span>
          <Button
            variant="ghost"
            onClick={() => setConfirmStatus(false)}
            disabled={busy}
          >
            Cancel
          </Button>
          <Button
            variant="danger"
            loading={busy}
            onClick={() => {
              setConfirmStatus(false);
              void saveStatus();
            }}
          >
            Confirm — apply {orgStatusMeta(statusDraft).label}
          </Button>
        </div>
      ) : (
        <Button
          variant={disruptive ? "danger" : "primary"}
          loading={busy}
          onClick={() => {
            if (disruptive) {
              setConfirmStatus(true);
              return;
            }
            void saveStatus();
          }}
        >
          Apply status
        </Button>
      )}
    </section>
  );
}
