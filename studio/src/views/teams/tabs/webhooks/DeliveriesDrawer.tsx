import { errorMessage } from "@/lib/errorHints";
import { useEffect, useState } from "react";

import {
  type WebhookConfig,
  type WebhookDelivery,
  listWebhookDeliveries,
} from "@/api/webhooks";
import { Button } from "@/components/ui/Button";
import { Dialog } from "@/components/ui/Dialog";
import { EmptyState } from "@/components/ui/EmptyState";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Table, THead, Th, TBody, Tr, Td, TableSkeleton } from "@/components/ui/Table";

import { DeliveryStatusBadge } from "./DeliveryStatusBadge";

// Side drawer that lists recent deliveries for one webhook. Refresh is
// manual (operator-triggered) — the spine already audits each delivery,
// this is a read-only window over that log.
export function DeliveriesDrawer({
  teamID,
  webhook,
  onClose,
}: {
  teamID: string;
  webhook: WebhookConfig;
  onClose: () => void;
}) {
  const [deliveries, setDeliveries] = useState<WebhookDelivery[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);

  const load = async () => {
    setLoading(true);
    setErr(null);
    try {
      setDeliveries(await listWebhookDeliveries(teamID, webhook.id));
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void load();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [teamID, webhook.id]);

  return (
    <Dialog
      open
      onOpenChange={(v) => {
        if (!v) onClose();
      }}
      title={`Deliveries — ${webhook.name}`}
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
        <TableSkeleton rows={4} cols={5} />
      ) : deliveries.length === 0 ? (
        <EmptyState message="No deliveries yet. Push an event from the forge to see it appear here." />
      ) : (
        <Table caption={`Recent deliveries for webhook ${webhook.name}`} density="sm">
          <THead>
            <Th>Status</Th>
            <Th>Received</Th>
            <Th>Event</Th>
            <Th>From</Th>
            <Th>Error</Th>
          </THead>
          <TBody>
            {deliveries.map((d) => (
              <Tr key={d.id}>
                <Td>
                  <DeliveryStatusBadge status={d.status} />
                </Td>
                <Td className="text-fg-muted whitespace-nowrap">
                  {new Date(d.received_at).toLocaleString()}
                </Td>
                <Td>
                  {d.event_kind ?? "—"}
                  {d.event_action ? ` / ${d.event_action}` : ""}
                </Td>
                <Td className="text-fg-muted">{d.source_ip ?? "—"}</Td>
                <Td className="text-danger">{d.error ?? "—"}</Td>
              </Tr>
            ))}
          </TBody>
        </Table>
      )}
    </Dialog>
  );
}
