// Credential-spend console (super-admin, cloud): what a single credential cost
// across every tenant it served — the cross-tenant view no team page can show
// (#641). Mirrors the gated-table pattern of the sibling admin consoles.
//
// The response separates metered_usd (a real invoice) from estimated_usd (what
// a subscription's calls WOULD have cost); the two are shown apart and never
// summed, per the API's `nature`. fingerprint and repo are mutually exclusive
// questions (the API 400s on both); the form enforces it and echoes the scope
// the answer actually covers so identical numbers across tenants can't be
// misread as a frozen meter.

import { useState } from "react";
import { keepPreviousData, useQuery, useQueryClient } from "@tanstack/react-query";

import { FeatureUnavailableError, getAdminCredentialUsage } from "@/api/adminCredUsage";
import { clearUsageReadings } from "@/api/adminUsageReadings";
import { errorMessage, toastError } from "@/lib/errorHints";

import { useAuth } from "@/auth/AuthContext";
import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";
import { FieldLabel } from "@/components/ui/FieldLabel";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { Table, THead, Th, TBody, Tr, Td, TableSkeleton } from "@/components/ui/Table";
import { CloudOnlyNotice } from "@/components/shared/CloudOnlyNotice";
import { useConfirm } from "@/hooks/useConfirm";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import { useServerInfoStore } from "@/store/serverInfo";
import { useUIStore } from "@/store/ui";

import AdminNav from "../AdminNav";
import {
  SPEND_TIERS,
  buildQuery,
  formatTokens,
  formatUSD,
  isValidMonth,
  type SpendTier,
} from "./credentialSpend";

export default function CredentialSpendPage() {
  const { user } = useAuth();
  const isSuper = user?.is_super_admin ?? false;
  const serverInfo = useServerInfoStore((s) => s.info);
  const isCloud = serverInfo?.mode === "cloud";

  // The APPLIED filters (what the query keys on) vs the draft form. Applying is
  // explicit (a button / Enter) so typing a fingerprint doesn't fire a request
  // per keystroke.
  const [tier, setTier] = useState<SpendTier>("platform");
  const [month, setMonth] = useState("");
  const [fingerprint, setFingerprint] = useState("");
  const [repo, setRepo] = useState("");
  const [applied, setApplied] = useState(() =>
    buildQuery({ tier: "platform", month: "", fingerprint: "", repo: "" }),
  );

  const query = useQuery({
    queryKey: ["admin-credential-usage", applied],
    queryFn: () => getAdminCredentialUsage(applied),
    enabled: isSuper && isCloud,
    placeholderData: keepPreviousData,
  });
  const view = query.data;
  const loaded = !query.isPending;
  const unavailable = query.error instanceof FeatureUnavailableError;

  const queryClient = useQueryClient();
  const addToast = useUIStore((s) => s.addToast);
  const { confirm, dialog: confirmDialog } = useConfirm();
  const [clearing, setClearing] = useState<string | null>(null);

  // Clear one credential's stored usage readings by fingerprint — the escape
  // hatch when a provider reset a window early and the stale reading keeps
  // refusing every run of that credential (admin_usage_readings_routes.go).
  const clearReadings = async (fp: string) => {
    const ok = await confirm({
      title: "Clear usage readings?",
      message: (
        <>
          Forget the stored usage-window readings for credential{" "}
          <code className="break-all">{fp}</code>. Use this only when a provider
          reset a window early and the stale reading is refusing runs pre-flight.
          It clears this one credential and leaves the global caps alone.
        </>
      ),
      confirmLabel: "Clear readings",
    });
    if (!ok) return;
    setClearing(fp);
    try {
      const res = await clearUsageReadings(fp);
      addToast(
        res.deleted > 0
          ? `Cleared ${res.deleted} reading(s) for ${fp}`
          : `No stored readings for ${fp} (nothing to clear)`,
        "success",
      );
      await queryClient.invalidateQueries({ queryKey: ["admin-credential-usage"] });
    } catch (e) {
      toastError(addToast, e, "Clear readings failed");
    } finally {
      setClearing(null);
    }
  };

  useHeaderSlot({
    left: <span className="text-sm font-semibold">Credential spend</span>,
    right: view ? (
      <span className="text-xs text-fg-muted">{view.credentials.length} credential(s)</span>
    ) : null,
  });

  if (!isSuper) {
    return (
      <div className="p-6">
        <p className="text-sm text-fg-muted">Super-admin only.</p>
      </div>
    );
  }

  if (serverInfo && !isCloud) {
    return (
      <div className="h-full overflow-auto">
        <div className="max-w-5xl mx-auto p-3 sm:p-6">
          <CloudOnlyNotice feature="Credential spend" />
        </div>
      </div>
    );
  }

  if (unavailable) {
    return (
      <div className="h-full overflow-auto">
        <div className="max-w-5xl mx-auto p-3 sm:p-6 space-y-4">
          <AdminNav />
          <EmptyState
            title="Per-credential usage not enabled"
            message="The /api/admin/credentials/usage endpoint isn't available on this server."
          />
        </div>
      </div>
    );
  }

  const monthValid = isValidMonth(month);
  const usingFingerprint = fingerprint.trim() !== "";
  const usingRepo = repo.trim() !== "";
  const fetchErr =
    query.error && !unavailable && !query.isFetching ? errorMessage(query.error) : null;

  const apply = () => {
    if (!monthValid) return;
    setApplied(buildQuery({ tier, month, fingerprint, repo }));
  };

  const scope = view?.scope;

  return (
    <div className="h-full overflow-auto">
      <div className="max-w-5xl mx-auto p-3 sm:p-6 space-y-4">
        <AdminNav />

        <p className="text-caption text-fg-subtle max-w-3xl">
          What each credential cost across every tenant it served. A metered key
          bills a real invoice; a subscription&apos;s figure is what its calls
          would have cost — the two are shown apart and must never be summed. Ask
          by tier, or narrow to one fingerprint or one repository (not both).
        </p>

        {/* Filters */}
        <section className="bg-surface-1 border border-border-subtle rounded-[var(--radius-lg)] p-3 flex flex-wrap items-end gap-3">
          <div className="w-40">
            <FieldLabel htmlFor="spend-tier">Tier</FieldLabel>
            <Select
              id="spend-tier"
              value={tier}
              disabled={usingFingerprint || usingRepo}
              onChange={(e) => setTier(e.target.value as SpendTier)}
            >
              {SPEND_TIERS.map((t) => (
                <option key={t} value={t}>
                  {t}
                </option>
              ))}
            </Select>
          </div>
          <div className="w-32">
            <FieldLabel htmlFor="spend-month">Month</FieldLabel>
            <Input
              id="spend-month"
              placeholder="YYYY-MM"
              error={!monthValid}
              value={month}
              onChange={(e) => setMonth(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && apply()}
            />
          </div>
          <div className="w-56">
            <FieldLabel htmlFor="spend-fingerprint">Fingerprint</FieldLabel>
            <Input
              id="spend-fingerprint"
              placeholder="one credential"
              disabled={usingRepo}
              value={fingerprint}
              onChange={(e) => setFingerprint(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && apply()}
            />
          </div>
          <div className="w-56">
            <FieldLabel htmlFor="spend-repo">Repository</FieldLabel>
            <Input
              id="spend-repo"
              placeholder="one repo"
              disabled={usingFingerprint}
              value={repo}
              onChange={(e) => setRepo(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && apply()}
            />
          </div>
          <Button size="sm" disabled={!monthValid} loading={query.isFetching} onClick={apply}>
            Apply
          </Button>
        </section>

        {!monthValid && (
          <p className="text-xs text-danger">Month must be YYYY-MM (or blank for the current month).</p>
        )}

        {fetchErr && (
          <InlineBanner tone="danger" layout="inline">
            {fetchErr}
          </InlineBanner>
        )}

        {/* Scope + totals — always say what the numbers cover. */}
        {view && (
          <div className="flex flex-wrap items-center gap-x-6 gap-y-1 text-caption text-fg-subtle">
            <span>
              Month: <span className="text-fg-default">{view.month}</span>
            </span>
            <span>
              Scope:{" "}
              <span className="text-fg-default">
                {scope?.fingerprint
                  ? `fingerprint ${scope.fingerprint}`
                  : scope?.repo
                    ? `repo ${scope.repo}`
                    : `tier ${scope?.tier ?? "—"}`}
              </span>
            </span>
            <span>
              Metered: <span className="text-fg-default">{formatUSD(view.metered_usd)}</span>
            </span>
            <span>
              Estimated: <span className="text-fg-default">{formatUSD(view.estimated_usd)}</span>
            </span>
          </div>
        )}

        <section className="bg-surface-1 border border-border-subtle rounded-[var(--radius-lg)] shadow-[var(--shadow-sm)] overflow-hidden">
          {!loaded ? (
            <div className="p-3">
              <TableSkeleton rows={5} cols={6} />
            </div>
          ) : !view || view.credentials.length === 0 ? (
            <EmptyState message="No spend recorded for this scope and month." />
          ) : (
            <Table caption="Per-credential spend">
              <THead>
                <Th>Fingerprint</Th>
                <Th>Provider</Th>
                <Th>Tier</Th>
                <Th>Nature</Th>
                <Th align="right">Cost</Th>
                <Th align="right">Runs</Th>
                <Th>Tokens</Th>
                <Th align="right">Actions</Th>
              </THead>
              <TBody>
                {view.credentials.map((c) => (
                  <Tr key={`${c.fingerprint}:${c.tenant_id ?? ""}:${c.repo_id ?? ""}`}>
                    <Td className="font-mono text-caption break-all">{c.fingerprint}</Td>
                    <Td className="text-fg-muted">{c.provider}</Td>
                    <Td className="text-fg-muted">{c.tier}</Td>
                    <Td>
                      <span
                        className={
                          c.nature === "metered" ? "text-fg-default" : "text-fg-muted"
                        }
                      >
                        {c.nature}
                      </span>
                    </Td>
                    <Td align="right">{formatUSD(c.cost_usd)}</Td>
                    <Td align="right">{c.runs}</Td>
                    <Td className="text-caption text-fg-muted">{formatTokens(c)}</Td>
                    <Td align="right" className="whitespace-nowrap">
                      <Button
                        size="sm"
                        variant="ghost"
                        loading={clearing === c.fingerprint}
                        disabled={clearing != null}
                        onClick={() => void clearReadings(c.fingerprint)}
                      >
                        Clear readings
                      </Button>
                    </Td>
                  </Tr>
                ))}
              </TBody>
            </Table>
          )}
        </section>
      </div>
      {confirmDialog}
    </div>
  );
}
