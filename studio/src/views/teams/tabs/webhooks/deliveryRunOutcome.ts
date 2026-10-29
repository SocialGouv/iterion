import type { WebhookDelivery } from "@/api/webhooks";
import type { RunOutcomeRecord } from "@/components/shared/LastRunOutcome";

/**
 * deliveryRunOutcome projects a delivery onto the RunOutcomeRecord every
 * record that launches runs renders through LastRunOutcome (#1501) — the
 * inbound delivery is the fourth kind, so it maps onto the shared component
 * instead of growing its own outcome markup.
 *
 * `run_id` is this record's id half (a delivery launches ONE run, and a retry
 * of a failed row replaces it), which is why it maps onto `last_run_id`.
 * `error` is deliberately NOT mapped onto `last_error`: the delivery's own
 * refusal already has its own column in the drawer, and rendering it twice
 * would read as two separate failures.
 */
export function deliveryRunOutcome(d: WebhookDelivery): RunOutcomeRecord {
  return {
    last_run_id: d.run_id,
    last_run_status: d.last_run_status,
    last_run_error: d.last_run_error,
    last_run_error_code: d.last_run_error_code,
    last_run_at: d.last_run_at,
  };
}
