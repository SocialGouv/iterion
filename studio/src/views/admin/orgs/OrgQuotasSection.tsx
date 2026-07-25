// Quota editor for one org: memory (GiB), monthly run quota, monthly cost
// cap. Owns the three numeric drafts (initialised from the org, kept for
// the drawer's lifetime) and saves through the shared run() slot.

import { useState } from "react";

import { gibToBytes, updateOrg, type OrgView } from "@/api/orgs";
import { Button } from "@/components/ui/Button";
import { Input } from "@/components/ui/Input";

import { Field } from "./orgFields";

export function OrgQuotasSection({
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
  // Quota draft state — initialised from org.
  const initialGiB = org.memory_quota_bytes ? org.memory_quota_bytes / (1 << 30) : 0;
  const [memGiB, setMemGiB] = useState<number>(initialGiB);
  const [monthlyRuns, setMonthlyRuns] = useState<number>(org.monthly_run_quota ?? 0);
  const [costCap, setCostCap] = useState<number>(org.monthly_cost_cap_usd ?? 0);

  const saveQuotas = () =>
    run(async () => {
      await updateOrg(org.id, {
        memory_quota_bytes: memGiB > 0 ? gibToBytes(memGiB) : 0,
        monthly_run_quota: monthlyRuns,
        monthly_cost_cap_usd: costCap,
      });
      await onAfterUpdate();
    });

  return (
    <section className="space-y-3 mb-6">
      <h4 className="font-medium">Quotas</h4>
      <div className="grid grid-cols-2 sm:grid-cols-3 gap-2">
        <Field label="Memory quota (GiB, 0 = default)">
          <Input
            type="number"
            min={0}
            step={0.5}
            value={String(memGiB)}
            onChange={(e) => setMemGiB(Number(e.target.value))}
          />
        </Field>
        <Field label="Monthly run quota (0 = unlimited)">
          <Input
            type="number"
            min={0}
            value={String(monthlyRuns)}
            onChange={(e) => setMonthlyRuns(Number(e.target.value))}
          />
        </Field>
        <Field label="Monthly cost cap USD (0 = unlimited)">
          <Input
            type="number"
            min={0}
            step={1}
            value={String(costCap)}
            onChange={(e) => setCostCap(Number(e.target.value))}
          />
        </Field>
      </div>
      <Button variant="primary" loading={busy} onClick={() => void saveQuotas()}>
        Save quotas
      </Button>
    </section>
  );
}
