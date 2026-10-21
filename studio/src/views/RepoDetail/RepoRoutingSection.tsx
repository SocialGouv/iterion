import { useState } from "react";

import { RoutingPolicyEditor } from "@/components/shared/RoutingPolicyEditor";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { updateForgeRepoBots, type ForgeIntegration } from "@/api/forgeConnections";
import { errorMessage } from "@/lib/errorHints";

// RepoRoutingSection is the routing block's repo-scoped editor: the
// integration record's own routing policy (ADR-129) — the binding level
// for every run this repo's webhook and schedules launch, durable across
// re-provisioning because the record is what Provision rebuilds the
// webhook config from. Same shared strict-JSON editor as the org/team
// levels; the server validates the schema (a 400 naming it), refuses
// locks (the binding level holds none) and parks org-gated teams.
export default function RepoRoutingSection({
  teamID,
  integration,
  canManage,
  onSaved,
}: {
  teamID: string;
  integration: ForgeIntegration;
  canManage: boolean;
  onSaved: () => Promise<void> | void;
}) {
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  // The response's operator-truth: a 202 parks the request in the org
  // approval queue (nothing was written server-side), and a direct write
  // carries the blast-radius warnings the server could not gate. Both are
  // invisible to an editor that discards the resolved result.
  const [parked, setParked] = useState<string | null>(null);
  const [warnings, setWarnings] = useState<string[]>([]);

  const save = async (routing: unknown) => {
    setBusy(true);
    setErr(null);
    setParked(null);
    setWarnings([]);
    try {
      // No bot set: an absent bot_ids keeps the stored one server-side,
      // while the page's snapshot would unbind whatever was bound since
      // it loaded (the same rule as the review-tier and merge-gate
      // switches). The routing write is WHOLESALE: null clears, an object
      // replaces — the saved JSON is the whole truth, never a patch.
      const res = await updateForgeRepoBots(
        teamID,
        integration.id,
        undefined,
        undefined,
        undefined,
        (routing as Record<string, unknown> | null) ?? null,
      );
      if (res.pending_approval) {
        setParked(`${res.detail ?? "the request waits in the org approval queue"} (${res.approval_id})`);
      } else {
        setWarnings(res.warnings ?? []);
      }
      await onSaved();
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <>
      {parked && (
        <InlineBanner tone="warning" title="Parked for org approval" layout="inline">
          {parked}
        </InlineBanner>
      )}
      {warnings.map((w, i) => (
        <InlineBanner key={i} tone="warning" layout="inline">
          {w}
        </InlineBanner>
      ))}
      <RoutingPolicyEditor
        heading="Routing policy"
        scopeLabel="repo level · ADR-129"
        description="The routing block every run this repo launches under — webhook deliveries and scheduled fires alike. JSON: an object to replace, null to clear. On an org that requires provisioning approval, saving parks the request for an org admin; the server refuses locks and validates the vocabulary."
        storedPolicy={integration.routing ?? null}
        canManage={canManage}
        busy={busy}
        saveError={err}
        onSave={save}
      />
    </>
  );
}
