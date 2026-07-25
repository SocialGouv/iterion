// OrgDrawer — the per-org management dialog: name/slug details, usage
// stats, quota editor, status editor, danger zone. Each section owns its
// drafts; every mutation rides the page's shared run() slot and refreshes
// the list via onAfterUpdate.

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";

import { updateOrg, type OrgView } from "@/api/orgs";
import { FeatureUnavailableError, getAdminOrgUsage } from "@/api/usage";
import { Button } from "@/components/ui/Button";
import { Dialog } from "@/components/ui/Dialog";
import { Input } from "@/components/ui/Input";
import { errorMessage } from "@/lib/errorHints";

import { OrgDangerZone } from "./OrgDangerZone";
import { OrgQuotasSection } from "./OrgQuotasSection";
import { OrgStatusSection } from "./OrgStatusSection";
import { OrgUsageStats } from "./OrgUsageStats";
import { Field } from "./orgFields";

export function OrgDrawer({
  org,
  busy,
  isActiveOrg,
  onClose,
  onChanged,
  onAfterUpdate,
  reloadIdentity,
  run,
}: {
  org: OrgView;
  busy: boolean;
  isActiveOrg: boolean;
  onClose: () => void;
  onChanged: (o: OrgView) => void;
  onAfterUpdate: () => Promise<void>;
  reloadIdentity: () => Promise<void>;
  run: (fn: () => Promise<unknown>) => Promise<void>;
}) {
  const usageQuery = useQuery({
    queryKey: ["admin-org-usage", org.id],
    queryFn: () => getAdminOrgUsage(org.id),
  });
  const usage = usageQuery.data ?? null;
  const usageErr = usageQuery.error
    ? usageQuery.error instanceof FeatureUnavailableError
      ? "Usage view not enabled."
      : errorMessage(usageQuery.error)
    : null;

  // Name + slug drafts.
  const [nameDraft, setNameDraft] = useState(org.name);
  const [slugDraft, setSlugDraft] = useState(org.slug);

  const saveDetails = () =>
    run(async () => {
      await updateOrg(org.id, { name: nameDraft.trim(), slug: slugDraft.trim() });
      await onAfterUpdate();
      // Reflect the new name/slug in the org switcher + breadcrumbs immediately.
      await reloadIdentity();
    });

  return (
    <Dialog
      open
      onOpenChange={(v) => {
        if (!v) onClose();
      }}
      title={org.name}
      description={
        <span>
          <span className="font-mono text-xs">{org.id}</span>
          {org.personal ? " · personal" : ""}
        </span>
      }
      widthClass="max-w-3xl"
      footer={
        <Button variant="secondary" onClick={onClose}>
          Close
        </Button>
      }
    >
      {usageErr && (
        <div className="text-sm text-fg-muted bg-warning-soft border border-warning/40 rounded px-3 py-2 mb-3">
          {usageErr}
        </div>
      )}

      <section className="space-y-3 mb-4">
        <h4 className="font-medium">Details</h4>
        <div className="grid grid-cols-1 sm:grid-cols-2 gap-2">
          <Field label="Name">
            <Input value={nameDraft} onChange={(e) => setNameDraft(e.target.value)} />
          </Field>
          <Field label="Slug (URL identifier)">
            <Input
              value={slugDraft}
              onChange={(e) => setSlugDraft(e.target.value)}
              placeholder="my-org"
            />
          </Field>
        </div>
        <Button
          variant="primary"
          loading={busy}
          disabled={
            nameDraft.trim() === "" ||
            (nameDraft.trim() === org.name && slugDraft.trim() === org.slug)
          }
          onClick={() => void saveDetails()}
        >
          Save details
        </Button>
      </section>

      <OrgUsageStats usage={usage} />
      <OrgQuotasSection org={org} busy={busy} run={run} onAfterUpdate={onAfterUpdate} />
      <OrgStatusSection org={org} busy={busy} run={run} onAfterUpdate={onAfterUpdate} />
      <OrgDangerZone
        org={org}
        busy={busy}
        isActiveOrg={isActiveOrg}
        run={run}
        onAfterUpdate={onAfterUpdate}
        reloadIdentity={reloadIdentity}
        onChanged={onChanged}
      />
    </Dialog>
  );
}
