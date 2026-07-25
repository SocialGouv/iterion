import {
  type WebhookConfig,
  type WebhookDelivery,
  listWebhookDeliveries,
} from "@/api/webhooks";
import { DeliveriesDrawer as SharedDeliveriesDrawer } from "@/components/shared/DeliveriesDrawer";
import { Td, Tr } from "@/components/ui/Table";
import { formatDateTime } from "@/lib/format";

import { DeliveryStatusBadge } from "./DeliveryStatusBadge";

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
      columns={["Status", "Received", "Event", "From", "Error"]}
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
          <Td className="text-danger">{d.error ?? "—"}</Td>
        </Tr>
      )}
      onClose={onClose}
    />
  );
}
