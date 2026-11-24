// Organizations table for the admin console — a row click (or its Open
// button) opens the org drawer.

import { fmtQuotaGiB, type OrgView } from "@/api/orgs";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";
import { Table, THead, Th, TBody, Tr, Td, TableSkeleton } from "@/components/ui/Table";
import { pointerRowProps } from "@/lib/a11y";

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
                  // Pointer convenience only: the row holds the Open
                  // button, so a role="button" here would be
                  // `nested-interactive` (WCAG 4.1.2) and would give one
                  // row two tab stops for one action. The Open button is
                  // the keyboard path (RGAA 7.3).
                  {...pointerRowProps(() => onOpen(o))}
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
                    {/* One Open per row, all reading "Open": the name
                        carries the org the button acts on, and the slug is
                        the row's own discriminator (two orgs can share a
                        display name). Visible word first, for SC 2.5.3. */}
                    <Button
                      size="sm"
                      variant="ghost"
                      aria-label={`Open ${o.name} ${o.slug}`}
                      onClick={(e) => { e.stopPropagation(); onOpen(o); }}
                    >
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
