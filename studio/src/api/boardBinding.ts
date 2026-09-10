// Team ⇄ forge project-board binding (ADR-097).
// Mirrors pkg/server/board_binding_routes.go.

import type { components } from "./schema";
import { is404, request } from "./client";

export type BoardBinding = components["schemas"]["BoardBinding"];

/** The PUT payload: the board's ADDRESS plus the policy — never its ids. */
export interface BindBoardInput {
  owner: string;
  number: number;
  /** "org" (default) or "user". */
  owner_kind?: string;
  connection_id: string;
  /** Overrides the shipped column vocabulary; must be injective. */
  status_map?: Record<string, string>;
  /** Reconciliation interval. Omitted = the server default; 0 = off. */
  sync_every_seconds?: number;
}

/**
 * getBoardBinding returns the team's binding, or null when it has none.
 *
 * A 404 is the ordinary "not bound yet" state — the majority case — so it
 * resolves to null and the card renders its empty state rather than an error
 * banner. It is deliberately NOT routed through guard404: that helper means
 * "this deployment does not have the feature", and here the two are the same
 * status code. A deployment genuinely missing the endpoint surfaces on the
 * first bind attempt, which says so plainly instead of a permanent empty card.
 */
export async function getBoardBinding(teamID: string): Promise<BoardBinding | null> {
  try {
    return await request<BoardBinding>(`/teams/${encodeURIComponent(teamID)}/board-binding`);
  } catch (e) {
    if (is404(e)) return null;
    throw e;
  }
}

export async function bindBoard(
  teamID: string,
  input: BindBoardInput,
): Promise<BoardBinding> {
  return request<BoardBinding>(`/teams/${encodeURIComponent(teamID)}/board-binding`, {
    method: "PUT",
    body: JSON.stringify(input),
  });
}

export async function unbindBoard(teamID: string): Promise<void> {
  await request<void>(`/teams/${encodeURIComponent(teamID)}/board-binding`, {
    method: "DELETE",
  });
}

/**
 * parseStatusMap parses the "Todo=ready,Doing=in_progress" form the input
 * accepts, mirroring the CLI's --status-map. Returns an error message rather
 * than throwing, so the form can render it inline.
 *
 * Strict on purpose: a pair silently dropped would leave that column unmapped
 * and inert — a binding that looks like it works until a column never syncs.
 */
export function parseStatusMap(
  raw: string,
): { map?: Record<string, string>; error?: string } {
  if (!raw.trim()) return {};
  const map: Record<string, string> = {};
  for (const pair of raw.split(",")) {
    const p = pair.trim();
    if (!p) return { error: `Empty entry in "${raw}" — expected Column=state pairs.` };
    const parts = p.split("=");
    if (parts.length !== 2) return { error: `"${p}" is not a Column=state pair.` };
    const col = (parts[0] ?? "").trim();
    const state = (parts[1] ?? "").trim();
    if (!col || !state) return { error: `"${p}" is not a Column=state pair.` };
    if (map[col] !== undefined) return { error: `Column "${col}" is named twice.` };
    map[col] = state;
  }
  return { map };
}

/** formatStatusMap renders a binding's effective map back into the input form. */
export function formatStatusMap(b: BoardBinding | null): string {
  if (!b?.status_mapping?.length) return "";
  return b.status_mapping.map((m) => `${m.status}=${m.state}`).join(",");
}
