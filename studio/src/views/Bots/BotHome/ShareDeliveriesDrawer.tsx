import {
  listConfigShareDeliveries,
  type Delivery,
  type ShareView,
} from "@/api/configShareAdmin";
import { DeliveriesDrawer } from "@/components/shared/DeliveriesDrawer";
import { StatusBadge, Td, Tr, type BadgeVariant } from "@/components/ui";
import { formatDateTime } from "@/lib/format";

// Edit history (the "delivery" audit) for one config-share: one row per SAVE
// made through the share — via the token link OR the signed-in config editor —
// with the before/after state, for forensics after a leak. NOT the bot's runs
// (those read the file from git directly). A thin parameterization of the
// shared deliveries drawer.
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
      title={`Edit history — ${shareName}`}
      caption={`Saves made through this share (token link or config editor) — not the bot's scheduled runs`}
      emptyMessage="No edits through this share yet. This logs saves made via the share link or the config editor — the bot's scheduled runs read the file straight from git and don't appear here."
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
