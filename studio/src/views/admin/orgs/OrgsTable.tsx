// Organizations table for the admin console — a row click (or its Open
// button) opens the org drawer.

import { fmtQuotaGiB, type OrgView } from "@/api/orgs";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";
import { Table, THead, Th, TBody, Tr, Td, TableSkeleton } from "@/components/ui/Table";
import { clickableRowProps } from "@/lib/a11y";

import { orgStatusMeta } from "./orgStatusMeta";

export function OrgsTable({
  orgs,
  loaded,
  onOpen,
}: {
  orgs: OrgView[];
  loaded: boolean;
  onOpen: (o: OrgView) => void;
}) {
  return (
    <section className="bg-surface-1 border border-border-subtle rounded-[var(--radius-lg)] shadow-[var(--shadow-sm)] overflow-hidden">
      {!loaded ? (
        <div className="p-3">
          <TableSkeleton rows={4} cols={5} />
        </div>
      ) : orgs.length === 0 ? (
        <EmptyState message="No organizations yet." />
      ) : (
        <Table caption="Organizations">
          <THead>
            <Th>Name</Th>
            <Th>Slug</Th>
            <Th>Status</Th>
            <Th>Memory quota</Th>
            <Th align="right">Manage</Th>
          </THead>
          <TBody>
            {orgs.map((o) => {
              const status = orgStatusMeta(o.status);
              return (
                <Tr
                  key={o.id}
                  className="cursor-pointer"
                  {...clickableRowProps(() => onOpen(o), `Open ${o.name} (${status.label})`)}
                >
                  <Td>
                    {o.name}
                    {o.personal && <span className="ml-2 text-xs text-fg-muted">personal</span>}
                  </Td>
                  <Td className="text-fg-muted">{o.slug}</Td>
                  <Td>
                    <Badge variant={status.variant}>{status.label}</Badge>
                  </Td>
                  <Td className="text-fg-muted">{fmtQuotaGiB(o.memory_quota_bytes)}</Td>
                  <Td align="right">
                    <Button size="sm" variant="ghost" onClick={(e) => { e.stopPropagation(); onOpen(o); }}>
                      Open
                    </Button>
                  </Td>
                </Tr>
              );
            })}
          </TBody>
        </Table>
      )}
    </section>
  );
}
