// The operator's legal pages (ADR-160): public reads, no session needed.

import { request } from "./client";
import type { LegalDocument } from "./types";

/** GET /api/legal/{slug} — one configured legal document as markdown. 404
 *  (ApiError) for a slug that names no document or one this deployment
 *  did not configure. */
export async function getLegalDocument(slug: string): Promise<LegalDocument> {
  return request(`/legal/${encodeURIComponent(slug)}`);
}
