// OrgTeamsSection — the super-admin org→teams drill-down inside OrgDrawer.
// Read-only: lists the teams in one org with their status and per-team
// executor caps. Mirrors GET /api/admin/orgs/{id}/teams (handleAdminOrgTeams),
// which had no UI consumer before. Team mutation stays out of this view (the
// org console governs orgs; team caps are edited from the org self-serve
// governance tab).

import { useQuery } from "@tanstack/react-query";

import { FeatureUnavailableError } from "@/api/client";
import { listAdminOrgTeams } from "@/api/orgs";
import { EmptyState } from "@/components/ui/EmptyState";
import { Table, THead, Th, TBody, Tr, Td, TableSkeleton } from "@/components/ui/Table";
import { errorMessage } from "@/lib/errorHints";

export function OrgTeamsSection({ orgID }: { orgID: string }) {
  const query = useQuery({
    queryKey: ["admin-org-teams", orgID],
    queryFn: () => listAdminOrgTeams(orgID),
  });
  const teams = query.data ?? [];
  const err =
    query.error && !(query.error instanceof FeatureUnavailableError)
      ? errorMessage(query.error)
      : null;

  const cap = (n: number | undefined) => (n && n > 0 ? String(n) : "—");

  return (
    <section className="space-y-2 mb-4">
      <h4 className="font-medium">
        Teams{" "}
        {!query.isPending && (
          <span className="text-caption text-fg-subtle">({teams.length})</span>
        )}
      </h4>

      {err && (
        <div className="text-sm text-fg-muted bg-warning-soft border border-warning/40 rounded px-3 py-2">
          {err}
        </div>
      )}

      {query.isPending ? (
        <TableSkeleton rows={3} cols={4} />
      ) : teams.length === 0 ? (
        <EmptyState message="This organization has no teams." />
      ) : (
        <Table caption="Teams in this organization">
          <THead>
            <Th>Name</Th>
            <Th>Status</Th>
            <Th align="right">Max concurrent</Th>
            <Th align="right">Launch rate /min</Th>
          </THead>
          <TBody>
            {teams.map((t) => (
              <Tr key={t.id}>
                <Td>
                  <div>{t.name}</div>
                  <div className="text-caption text-fg-subtle font-mono">
                    {t.slug}
                    {t.personal ? " · personal" : ""}
                  </div>
                </Td>
                <Td className="text-fg-muted">{t.status}</Td>
                <Td align="right">{cap(t.max_concurrent_runs)}</Td>
                <Td align="right">{cap(t.launch_rate_per_min)}</Td>
              </Tr>
            ))}
          </TBody>
        </Table>
      )}
    </section>
  );
}
