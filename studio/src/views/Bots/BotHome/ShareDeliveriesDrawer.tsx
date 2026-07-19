import { useEffect, useState } from "react";

import {
  listConfigShareDeliveries,
  type Delivery,
  type ShareView,
} from "@/api/configShareAdmin";
import {
  Button,
  Dialog,
  EmptyState,
  InlineBanner,
  StatusBadge,
  Table,
  TableSkeleton,
  TBody,
  Td,
  Th,
  THead,
  Tr,
  type BadgeVariant,
} from "@/components/ui";
import { errorMessage } from "@/lib/errorHints";
import { formatDateTime } from "@/lib/format";

// Drawer listing recent deliveries (editor-side requests) for one
// config-share. Refresh is manual (operator-triggered) — the server already
// audits each delivery, this is a read-only window over that log.
export function ShareDeliveriesDrawer({
  teamID,
  share,
  onClose,
}: {
  teamID: string;
  share: ShareView;
  onClose: () => void;
}) {
  const [deliveries, setDeliveries] = useState<Delivery[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);

  const load = async () => {
    setLoading(true);
    setErr(null);
    try {
      setDeliveries(await listConfigShareDeliveries(teamID, share.id));
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [teamID, share.id]);

  const shareName = share.label?.trim() || share.id;

  return (
    <Dialog
      open
      onOpenChange={(v) => {
        if (!v) onClose();
      }}
      title={`Deliveries — ${shareName}`}
      widthClass="max-w-3xl"
      footer={
        <>
          <Button variant="secondary" onClick={() => void load()}>
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
        <TableSkeleton rows={4} cols={6} />
      ) : deliveries.length === 0 ? (
        <EmptyState message="No deliveries yet — this share hasn't been fetched." />
      ) : (
        <Table caption={`Recent deliveries for share ${shareName}`} density="sm">
          <THead>
            <Th>Status</Th>
            <Th>When</Th>
            <Th>Method</Th>
            <Th>Actor</Th>
            <Th>Changed</Th>
            <Th>Error</Th>
          </THead>
          <TBody>
            {deliveries.map((d) => (
              <Tr key={d.id}>
                <Td>
                  <StatusBadge
                    variant={deliveryVariant(d.status)}
                    label={String(d.status)}
                  />
                </Td>
                <Td className="text-fg-muted whitespace-nowrap">
                  {formatDateTime(d.at)}
                </Td>
                <Td className="font-mono">{d.method}</Td>
                <Td className="text-fg-muted">
                  {formatActor(d)}
                </Td>
                <Td className="font-mono text-caption">
                  {d.changed_paths?.length ? d.changed_paths.join(", ") : "—"}
                </Td>
                <Td className="text-danger">{d.error || "—"}</Td>
              </Tr>
            ))}
          </TBody>
        </Table>
      )}
    </Dialog>
  );
}

// Badge variant for a delivery's HTTP status code.
function deliveryVariant(status: number): BadgeVariant {
  if (status >= 200 && status < 300) return "success";
  if (status >= 400) return "danger";
  return "neutral";
}

// Actor is "share:<id>" (token edit) or "user:<id>" (authenticated editor
// session); legacy rows have none — fall back to the source IP.
function formatActor(d: Delivery): string {
  if (d.actor) return d.actor;
  return d.source_ip || "—";
}
