import type { ForgeConnectionStatus, ForgeKind } from "@/api/forgeConnections";

// Friendly labels for the forge-connection enums the API returns as raw
// strings. Unknown values (a newer server) fall back to the raw value.

const STATUS_LABELS: Record<ForgeConnectionStatus, string> = {
  active: "Connected",
  needs_reauth: "Needs re-auth",
  revoked: "Revoked",
};

export function connectionStatusLabel(status: ForgeConnectionStatus): string {
  return STATUS_LABELS[status] ?? status;
}

const KIND_LABELS: Record<ForgeKind, string> = {
  oauth_app: "OAuth",
  github_app: "GitHub App",
  pat: "Personal token",
};

export function connectionKindLabel(kind: ForgeKind): string {
  return KIND_LABELS[kind] ?? kind;
}
