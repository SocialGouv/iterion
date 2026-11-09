import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";

import { Button } from "@/components/ui/Button";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { Table, THead, Th, TBody, Tr, Td } from "@/components/ui/Table";
import { errorMessage } from "@/lib/errorHints";
import { formatDateTime } from "@/lib/format";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Spinner } from "@/components/ui/Spinner";
import {
  type KeyPoolEntry,
  type PoolAttachmentPut,
  getOrgPoolAttachment,
  getTeamPoolAttachment,
  listPoolEntries,
  putOrgPoolAttachment,
  putTeamPoolAttachment,
} from "@/api/pools";

// PoolSection is the org-level and team-level editor of the key-pool
// attachment (ADR-164): the stored opt-in (which pool serves this scope's
// runs, with its fallback strategy and borrower draw caps), its CAS stamp,
// and the entries' derived status — the honest per-shape signal the launch
// path skips on ("reactive — no provider telemetry" for the shapes that
// carry none). Reduced motion: no transitions anywhere in this section.
export default function PoolSection({
  scope,
  id,
  canManage,
}: {
  scope: "org" | "team";
  id: string;
  canManage: boolean;
}) {
  const queryClient = useQueryClient();
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [poolID, setPoolID] = useState("");
  const [mode, setMode] = useState("priority");
  const [margin, setMargin] = useState("85");

  const getAttachment = scope === "org" ? getOrgPoolAttachment : getTeamPoolAttachment;
  const putAttachment = scope === "org" ? putOrgPoolAttachment : putTeamPoolAttachment;

  const query = useQuery({
    queryKey: [`${scope}-pool-attachment`, id],
    queryFn: () => getAttachment(id),
  });
  const view = query.data ?? null;

  const poolIDAttached = view?.pool_id ?? "";
  const entriesQuery = useQuery({
    queryKey: [`${scope}-pool-entries`, id, poolIDAttached],
    queryFn: () => listPoolEntries(poolIDAttached),
    enabled: poolIDAttached !== "",
  });

  if (query.isLoading) return <Spinner />;
  if (query.isError) {
    return <InlineBanner tone="danger">{errorMessage(query.error)}</InlineBanner>;
  }
  // An older backend (404 → null): the feature is genuinely absent there —
  // the section stays hidden rather than render a save that would fail.
  if (query.data === null) return null;

  const save = async () => {
    setBusy(true);
    setErr(null);
    try {
      const body: PoolAttachmentPut = {
        pool_id: poolID,
        enabled: true,
        mode,
        expected_updated_at: view?.updated_at,
      };
      const m = parseFloat(margin);
      if (!Number.isNaN(m)) body.margin_pct = m;
      await putAttachment(id, body);
      await queryClient.invalidateQueries({ queryKey: [`${scope}-pool-attachment`, id] });
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const detach = async () => {
    setBusy(true);
    setErr(null);
    try {
      if (scope === "org") {
        const { deleteOrgPoolAttachment: del } = await import("@/api/pools");
        await del(id, view?.updated_at);
      } else {
        const { deleteTeamPoolAttachment: del } = await import("@/api/pools");
        await del(id, view?.updated_at);
      }
      await queryClient.invalidateQueries({ queryKey: [`${scope}-pool-attachment`, id] });
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="rounded-lg border border-border-control bg-surface-0 p-4">
      <h3 className="text-sm font-semibold text-fg-default">Key pool</h3>
      <p className="mt-1 text-xs text-fg-muted">
        The named pool this {scope}'s runs draw from when an explicit credential does not fill a
        slot — never displacing one. Opt-in: detach to inherit; Enabled=false pauses. ADR-164.
      </p>
      {err && <InlineBanner tone="danger">{err}</InlineBanner>}
      {view === null ? (
        canManage && (
          <div className="mt-3 flex flex-wrap items-end gap-2">
            <label className="block text-xs text-fg-muted">
              Pool id
              <Input
                size="sm"
                value={poolID}
                onChange={(e) => setPoolID(e.target.value)}
                placeholder="pool-…"
                aria-label="Pool id to attach"
              />
            </label>
            <label className="block text-xs text-fg-muted">
              Mode
              <Select
                size="sm"
                value={mode}
                onChange={(e) => setMode(e.target.value)}
                aria-label="Strategy mode"
              >
                <option value="priority">priority</option>
                <option value="least_consumed">least_consumed</option>
              </Select>
            </label>
            <label className="block text-xs text-fg-muted">
              Margin %
              <Input
                size="sm"
                value={margin}
                onChange={(e) => setMargin(e.target.value)}
                aria-label="Pre-exhaustion margin percent"
              />
            </label>
            <Button variant="primary" loading={busy} disabled={poolID === ""} onClick={() => void save()}>
              Attach
            </Button>
          </div>
        )
      ) : (
        <div className="mt-3">
          <div className="flex flex-wrap items-center gap-3 text-sm text-fg-default">
            <span className="font-medium">{view.pool_id}</span>
            <span className={view.enabled ? "text-fg-default" : "text-fg-muted"}>
              {view.enabled ? "attached" : "paused (blocks inheritance)"}
            </span>
            <span className="text-fg-muted">
              mode {view.strategy.mode || "priority"} · margin {view.strategy.margin_pct ?? 85}%
            </span>
            {(view.limits.max_runs_per_day ?? 0) > 0 && (
              <span className="text-fg-muted">≤ {view.limits.max_runs_per_day} runs/day</span>
            )}
            {(view.limits.max_usd_per_day ?? 0) > 0 && (
              <span className="text-fg-muted">≤ ${view.limits.max_usd_per_day}/day</span>
            )}
          </div>
          <p className="mt-1 text-xs text-fg-muted">{formatDateTime(view.updated_at ?? "")}</p>
          <PoolEntries entries={entriesQuery.data ?? []} />
          {canManage && (
            <Button
              variant="secondary"
              loading={busy}
              onClick={() => void detach()}
              className="mt-3"
            >
              Detach (inherit)
            </Button>
          )}
        </div>
      )}
    </section>
  );
}

function PoolEntries({ entries }: { entries: KeyPoolEntry[] }) {
  if (entries.length === 0) return null;
  return (
    <Table caption="Pool entries and their derived status" density="sm">
      <THead>
        <tr>
          <Th>entry</Th>
          <Th>model</Th>
          <Th>prio</Th>
          <Th>status</Th>
        </tr>
      </THead>
      <TBody>
        {entries.map((e) => (
          <Tr key={e.id}>
            <Td className="font-mono">
              {e.source}/{e.ref}
            </Td>
            <Td>{e.model || "—"}</Td>
            <Td>{e.priority}</Td>
            <Td title={e.health_detail || ""} className="text-fg-muted">
              {e.status}
              {e.status === "cooling" && e.cooldown_until
                ? ` · resets ${formatDateTime(e.cooldown_until)}`
                : ""}
            </Td>
          </Tr>
        ))}
      </TBody>
    </Table>
  );
}
