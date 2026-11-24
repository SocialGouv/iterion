import {
  type WebhookConfig,
  type WebhookDelivery,
  listWebhookDeliveries,
} from "@/api/webhooks";
import { DeliveriesDrawer as SharedDeliveriesDrawer } from "@/components/shared/DeliveriesDrawer";
import LastRunOutcome from "@/components/shared/LastRunOutcome";
import { Td, Tr } from "@/components/ui/Table";
import { formatDateTime } from "@/lib/format";

import { DeliveryStatusBadge } from "./DeliveryStatusBadge";
import { deliveryRunOutcome } from "./deliveryRunOutcome";

// Recent deliveries for one webhook — a thin parameterization (fetch +
// columns/row shape) of the shared deliveries drawer.
export function DeliveriesDrawer({
  teamID,
  webhook,
  onClose,
}: {
  teamID: string;
  webhook: WebhookConfig;
  onClose: () => void;
}) {
  return (
    <SharedDeliveriesDrawer
      title={`Deliveries — ${webhook.name}`}
      caption={`Recent deliveries for webhook ${webhook.name}`}
      emptyMessage="No deliveries yet. Push an event from the forge to see it appear here."
      queryKey={["webhook-deliveries", teamID, webhook.id]}
      fetcher={() => listWebhookDeliveries(teamID, webhook.id)}
      // "Run" renders what the run DID (#1501). It is a column of its own
      // next to the delivery's own Status/Error: a delivery that launched and
      // whose run died read exactly like one that launched and worked.
      columns={["Status", "Received", "Event", "From", "Run", "Error"]}
      renderRow={(d: WebhookDelivery) => (
        <Tr key={d.id}>
          <Td>
            <DeliveryStatusBadge status={d.status} />
          </Td>
          <Td className="text-fg-muted whitespace-nowrap">
            {formatDateTime(d.received_at)}
          </Td>
          <Td>
            {d.event_kind ?? "—"}
            {d.event_action ? ` / ${d.event_action}` : ""}
          </Td>
          <Td className="text-fg-muted">{d.source_ip ?? "—"}</Td>
          <Td>
            {d.last_run_status ? (
              <LastRunOutcome record={deliveryRunOutcome(d)} />
            ) : (
              "—"
            )}
          </Td>
          <Td className="text-danger">{d.error ?? "—"}</Td>
        </Tr>
      )}
      onClose={onClose}
    />
  );
}
