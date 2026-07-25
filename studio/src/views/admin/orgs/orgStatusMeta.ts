import type { BadgeVariant } from "@/components/ui/Badge";

// Friendly label + badge tone for the org-status enum (the human
// descriptions live with the status Select in OrgStatusSection). Unknown
// values fall back to the raw enum with a neutral tone.
const ORG_STATUS_META: Record<string, { label: string; variant: BadgeVariant }> = {
  active: { label: "Active", variant: "success" },
  suspended: { label: "Suspended", variant: "danger" },
  read_only: { label: "Read-only", variant: "warning" },
  pending_deletion: { label: "Pending deletion", variant: "danger" },
};

export function orgStatusMeta(status: string): { label: string; variant: BadgeVariant } {
  return ORG_STATUS_META[status] ?? { label: status, variant: "neutral" };
}
