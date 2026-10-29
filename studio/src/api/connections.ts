import { guard404 } from "./client";
import type { components } from "./schema";
import { apiDelete, apiGet, apiPatch, apiPost } from "./typed";

// The connector-connection CRUD (ADR-098, #2759): a tenant's pkg/connection
// records, served by /api/teams/{id}/connections. Distinct from
// api/forgeConnections.ts (the forge integration layer): a CONNECTION here
// binds a credential to a connector package — what a `.bot`'s
// `connection: <alias>` and the agent MCP facade resolve.

// ConnectorConnection is the spec's connectionDTO. The credential is
// write-only server-side, so no field of it can carry one; `warnings` is the
// server's own advice (an inert grant, a cleartext origin) rendered verbatim.
export type ConnectorConnection = components["schemas"]["connectionDTO"];
export type ConnectorConnectionCreate = components["schemas"]["connectionCreateReq"];
export type ConnectorConnectionPatch = components["schemas"]["connectionPatchReq"];

export async function listConnections(
  teamID: string,
  connector?: string,
): Promise<ConnectorConnection[]> {
  const r = await guard404("connections", () =>
    apiGet("/api/teams/{id}/connections", {
      params: { id: teamID, ...(connector ? { connector } : {}) },
    }),
  );
  return (r.connections ?? []) as ConnectorConnection[];
}

export async function createConnection(
  teamID: string,
  input: ConnectorConnectionCreate,
): Promise<ConnectorConnection> {
  return (await apiPost("/api/teams/{id}/connections", {
    params: { id: teamID },
    body: input,
  })) as ConnectorConnection;
}

export async function patchConnection(
  teamID: string,
  connID: string,
  patch: ConnectorConnectionPatch,
): Promise<ConnectorConnection> {
  return (await apiPatch("/api/teams/{id}/connections/{conn_id}", {
    params: { id: teamID, conn_id: connID },
    body: patch,
  })) as ConnectorConnection;
}

// revokeConnection suspends the connection in place (status: revoked); the
// record — and what it was granted — stays auditable. deleteConnection removes
// it outright.
export async function revokeConnection(
  teamID: string,
  connID: string,
  reason?: string,
): Promise<ConnectorConnection> {
  return patchConnection(teamID, connID, { status: "revoked", status_reason: reason });
}

export async function deleteConnection(teamID: string, connID: string): Promise<void> {
  await apiDelete("/api/teams/{id}/connections/{conn_id}", {
    params: { id: teamID, conn_id: connID },
  });
}
