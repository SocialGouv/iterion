import {
  listConfigShareDeliveries,
  type Delivery,
  type ShareView,
} from "@/api/configShareAdmin";
import { DeliveriesDrawer } from "@/components/shared/DeliveriesDrawer";
import { StatusBadge, Td, Tr, type BadgeVariant } from "@/components/ui";
import { formatDateTime } from "@/lib/format";

// Recent deliveries (editor-side requests) for one config-share — a thin
// parameterization (fetch + columns/row shape) of the shared deliveries
// drawer.
export function ShareDeliveriesDrawer({
  teamID,
  share,
  onClose,
}: {
  teamID: string;
  share: ShareView;
  onClose: () => void;
}) {
  const shareName = share.label?.trim() || share.id;

  return (
    <DeliveriesDrawer
      title={`Deliveries — ${shareName}`}
      caption={`Recent deliveries for share ${shareName}`}
      emptyMessage="No deliveries yet — this share hasn't been fetched."
      queryKey={["config-share-deliveries", teamID, share.id]}
      fetcher={() => listConfigShareDeliveries(teamID, share.id)}
      columns={["Status", "When", "Method", "Actor", "Changed", "Error"]}
      renderRow={(d: Delivery) => (
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
          <Td className="text-fg-muted">{formatActor(d)}</Td>
          <Td className="font-mono text-caption">
            {d.changed_paths?.length ? d.changed_paths.join(", ") : "—"}
          </Td>
          <Td className="text-danger">{d.error || "—"}</Td>
        </Tr>
      )}
      onClose={onClose}
    />
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
