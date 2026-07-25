import type { ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";

import { Button } from "@/components/ui/Button";
import { Dialog } from "@/components/ui/Dialog";
import { EmptyState } from "@/components/ui/EmptyState";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Table, TBody, TableSkeleton, Th, THead } from "@/components/ui/Table";
import { errorMessage } from "@/lib/errorHints";

// Generic side drawer listing recent deliveries for one resource (a
// webhook, a config-share, …). Refresh is manual (operator-triggered) —
// the server already audits each delivery, this is a read-only window
// over that log. Call sites parameterize the fetch (queryKey + fetcher)
// and the table shape (columns + row rendering); the drawer owns the
// shared chrome: Dialog, skeleton, empty state, error banner, footer.
//
// Call sites mount the drawer only while it is open, so the query only
// fetches while the drawer shows.
export function DeliveriesDrawer<T>({
  title,
  caption,
  emptyMessage,
  queryKey,
  fetcher,
  columns,
  renderRow,
  onClose,
}: {
  title: string;
  caption: string;
  emptyMessage: string;
  queryKey: readonly unknown[];
  fetcher: () => Promise<T[]>;
  columns: string[];
  // Returns a keyed <Tr> for one delivery.
  renderRow: (item: T) => ReactNode;
  onClose: () => void;
}) {
  const query = useQuery({ queryKey, queryFn: fetcher });
  const deliveries = query.data ?? [];
  // isFetching (not isLoading) so the manual Refresh and a reopen both
  // show the skeleton — every load is an explicit, visible reload here.
  const loading = query.isFetching;
  const err = query.error && !loading ? errorMessage(query.error) : null;

  return (
    <Dialog
      open
      onOpenChange={(v) => {
        if (!v) onClose();
      }}
      title={title}
      widthClass="max-w-3xl"
      footer={
        <>
          <Button variant="secondary" onClick={() => void query.refetch()}>
            Refresh
          </Button>
          <Button variant="primary" onClick={onClose}>
            Close
          </Button>
        </>
      }
    >
      {err && (
        <InlineBanner tone="danger" layout="inline" className="mb-3">
          {err}
        </InlineBanner>
      )}
      {loading ? (
        <TableSkeleton rows={4} cols={columns.length} />
      ) : deliveries.length === 0 ? (
        <EmptyState message={emptyMessage} />
      ) : (
        <Table caption={caption} density="sm">
          <THead>
            {columns.map((c) => (
              <Th key={c}>{c}</Th>
            ))}
          </THead>
          <TBody>{deliveries.map((d) => renderRow(d))}</TBody>
        </Table>
      )}
    </Dialog>
  );
}
