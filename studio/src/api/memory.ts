// Shared-memory REST client. Mirrors pkg/server/memory_routes.go +
// pkg/knowledge/iface.go. Spaces are addressed by query params; the
// tenant/user come from the auth identity (cookie), never the URL.

import { ApiError, FeatureUnavailableError, extractErrorMessage, guard404, request } from "./client";
import { apiBase } from "@/lib/scope";

export { FeatureUnavailableError };

export type MemoryVisibility =
  | "private"
  | "bot"
  | "project"
  | "cross_project"
  | "user"
  | "org"
  | "global";

export interface MemorySpaceRef {
  name: string;
  visibility: MemoryVisibility;
  bot?: string; // required when visibility==="bot"
  project?: string; // optional ProjectID qualifier
}

export interface MemoryUsage {
  used_bytes: number;
  quota_bytes: number;
}

export interface MemoryDocumentMeta {
  path: string;
  title?: string;
  description?: string;
  tags?: string[];
  size: number;
  checksum?: string;
  revision?: number;
  updated_by?: string;
  updated_at?: string;
  blob_key?: string;
}

const BASE_URL = apiBase().replace(/\/$/, "");

function spaceQuery(ref: MemorySpaceRef, extra?: Record<string, string>): string {
  const sp = new URLSearchParams();
  sp.set("name", ref.name);
  sp.set("visibility", ref.visibility);
  if (ref.bot) sp.set("bot", ref.bot);
  if (ref.project) sp.set("project", ref.project);
  if (extra) for (const [k, v] of Object.entries(extra)) sp.set(k, v);
  return sp.toString();
}

export function getMemoryUsage(ref: MemorySpaceRef): Promise<MemoryUsage> {
  return guard404("memory", () => request<MemoryUsage>(`/memory/usage?${spaceQuery(ref)}`));
}

export async function listMemoryDocuments(
  ref: MemorySpaceRef,
  dir?: string,
): Promise<MemoryDocumentMeta[]> {
  return guard404("memory", async () => {
    const extra = dir ? { dir } : undefined;
    const r = await request<{ documents: MemoryDocumentMeta[] }>(
      `/memory/docs?${spaceQuery(ref, extra)}`,
    );
    return r.documents ?? [];
  });
}

// readMemoryDocument fetches the raw markdown body (Content-Type:
// text/markdown). The shared `request()` wrapper assumes JSON, so we
// hit fetch directly here.
export async function readMemoryDocument(
  ref: MemorySpaceRef,
  path: string,
): Promise<string> {
  const url = `${BASE_URL}/memory/doc?${spaceQuery(ref, { path })}`;
  const res = await fetch(url, { credentials: "include" });
  if (res.status === 404) {
    throw new FeatureUnavailableError("memory", `document ${path} not found`);
  }
  if (!res.ok) {
    throw new ApiError(res.status, `API error ${res.status}: ${await extractErrorMessage(res)}`);
  }
  return res.text();
}

export function memoryExportURL(ref: MemorySpaceRef): string {
  return `${BASE_URL}/memory/export?${spaceQuery(ref)}`;
}
