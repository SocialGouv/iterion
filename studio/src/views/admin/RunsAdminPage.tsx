import { useState } from "react";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";

import { listAdminOrgs, listAdminRuns } from "@/api/adminRuns";
import { useAuth } from "@/auth/AuthContext";
import { CloudOnlyNotice } from "@/components/shared/CloudOnlyNotice";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";
import { Table, TableSkeleton, TBody, Td, Th, THead, Tr } from "@/components/ui/Table";
import { errorMessage } from "@/lib/errorHints";
import { formatDateTime } from "@/lib/format";
import { studioRoute } from "@/lib/scope";
import { useServerInfoStore } from "@/store/serverInfo";

import AdminNav from "./AdminNav";

const PAGE = 50;

// The exact statuses the server filters on (store.RunStatus). The empty
// selection is "all states".
const STATUS_OPTIONS = [
  "running",
  "queued",
  "paused_waiting_human",
  "paused_operator",
  "finished",
  "failed",
  "failed_resumable",
  "cancelled",
] as const;

// Runs across every organization (#2148): the instance super-admin's one
// view of every org's runs — filterable by org and state (the filters
// combine server-side), paginated server-side across orgs by a keyset
// cursor. Read-only: a row opens the run through the ordinary /runs/:id
// route, whose choke point serves the super-admin from the RUN's team (no
// active-org error) and audits the cross-team grant. Rows bound to a
// sovereign runner pool (#2029) stay visually marked (A19): instance
// reach crosses every boundary by default, so the boundary is shown where
// it is crossed.
export default function RunsAdminPage() {
  const { user: me } = useAuth();
  const isSuper = me?.is_super_admin ?? false;
  const serverInfo = useServerInfoStore((s) => s.info);
  const isCloud = serverInfo?.mode === "cloud";
  useHeaderSlot({
    left: <span className="text-sm font-semibold">Runs, every organization</span>,
  });

  const [orgId, setOrgId] = useState("");
  const [status, setStatus] = useState("");

  const query = useInfiniteQuery({
    queryKey: ["admin-runs", orgId, status],
    queryFn: ({ pageParam }) =>
      listAdminRuns({
        orgId: orgId || undefined,
        status: status || undefined,
        cursor: pageParam || undefined,
        limit: PAGE,
      }),
    initialPageParam: "",
    getNextPageParam: (last) => (last.next_cursor ? last.next_cursor : undefined),
    enabled: isSuper && isCloud,
  });
  const orgs = useQuery({
    queryKey: ["admin-runs-orgs"],
    queryFn: listAdminOrgs,
    enabled: isSuper && isCloud,
  });

  if (!isSuper) {
    return (
      <div className="p-6">
        <p className="text-sm text-fg-muted">Super-admin only.</p>
      </div>
    );
  }
  // Deliberate local-mode gate: no fetch fired. While server_info is still
  // loading we fall through to the skeleton instead of flashing this notice
  // on cloud.
  if (serverInfo && !isCloud) {
    return (
      <div className="h-full overflow-auto">
        <div className="max-w-6xl mx-auto p-3 sm:p-6">
          <CloudOnlyNotice feature="Runs across every organization" />
        </div>
      </div>
    );
  }

  const rows = query.data?.pages.flatMap((p) => p.runs) ?? [];
  return (
    <div className="h-full overflow-auto">
      <div className="max-w-6xl mx-auto p-3 sm:p-6 space-y-4">
        <AdminNav />
        <p className="text-sm text-fg-muted">
          Every organization's runs in one view — the scope is the whole
          instance. Opening a run shows it in its own team's context.
        </p>
        <div className="flex flex-wrap items-center gap-3">
          <label className="text-sm text-fg-muted" htmlFor="admin-runs-org">
            Organization
          </label>
          <select
            id="admin-runs-org"
            className="rounded border border-border bg-surface px-2 py-1 text-sm"
            value={orgId}
            onChange={(e) => setOrgId(e.target.value)}
          >
            <option value="">All organizations</option>
            {(orgs.data ?? []).map((o) => (
              <option key={o.id} value={o.id}>
                {o.name}
              </option>
            ))}
          </select>
          <label className="text-sm text-fg-muted" htmlFor="admin-runs-status">
            State
          </label>
          <select
            id="admin-runs-status"
            className="rounded border border-border bg-surface px-2 py-1 text-sm"
            value={status}
            onChange={(e) => setStatus(e.target.value)}
          >
            <option value="">All states</option>
            {STATUS_OPTIONS.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>
        </div>

        {query.error != null && (
          <p className="text-sm text-danger">{errorMessage(query.error)}</p>
        )}
        {(query.isPending || serverInfo == null) && <TableSkeleton rows={5} cols={6} />}
        {serverInfo != null && !query.isPending && rows.length === 0 && query.error == null && (
          <EmptyState
            title="No runs match"
            message="No run in the selected scope yet."
          />
        )}
        {!query.isPending && rows.length > 0 && (
          <Table caption="Runs across every organization">
            <THead>
              <Th>Run</Th>
              <Th>Workflow</Th>
              <Th>State</Th>
              <Th>Team</Th>
              <Th>Organization</Th>
              <Th>Started</Th>
            </THead>
            <TBody>
              {rows.map((row) => (
                <Tr key={row.id}>
                  <Td>
                    <a
                      className="font-mono text-xs underline underline-offset-2"
                      href={studioRoute(`/runs/${row.id}`)}
                    >
                      {row.name || row.id}
                    </a>
                    {row.runner_pool ? (
                      <span
                        className="ml-2 rounded bg-warning/15 px-1 py-0.5 text-[10px] font-medium text-warning"
                        title={`Sovereign runner pool: ${row.runner_pool}`}
                      >
                        pool: {row.runner_pool}
                      </span>
                    ) : null}
                  </Td>
                  <Td>{row.workflow_name}</Td>
                  <Td>{row.status}</Td>
                  <Td>{row.team_name || row.team_id || "—"}</Td>
                  <Td>{row.org_name || "—"}</Td>
                  <Td>{formatDateTime(row.created_at)}</Td>
                </Tr>
              ))}
            </TBody>
          </Table>
        )}
        {query.hasNextPage && (
          <Button
            size="sm"
            disabled={query.isFetching}
            onClick={() => void query.fetchNextPage()}
          >
            Load older runs
          </Button>
        )}
      </div>
    </div>
  );
}
