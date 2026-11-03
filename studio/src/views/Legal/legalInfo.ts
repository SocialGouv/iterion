// What the legal surface reads from /api/server/info, and the French labels
// the pages print — one table for every site that names an accessibility
// status (footer, sidebar, the statement page itself).

import type { LegalAccessibility, LegalInfo } from "@/api/types";
import { rootRoute } from "@/lib/scope";
import { useServerInfoStore } from "@/store/serverInfo";

// The address of a legal page: a single root segment, outside /studio, so an
// operator signed in or not reaches the same page (App.tsx ROOT_SIDE_DOORS).
export const LEGAL_BASE = "/legal";

export function legalHref(slug: string): string {
  return `${LEGAL_BASE}/${slug}`;
}

/** The wouter target of a legal page. Every legal link goes through it: the
 *  same links render under the root router (product home, sign-in card) and
 *  inside the studio's <Router base="/studio"> (sidebar, plan du site,
 *  restricted shells), where a plain "/legal/<slug>" would resolve to
 *  /studio/legal/<slug> — a route the studio does not have. rootRoute's "~"
 *  drops every base and puts back the workspace pane's scope only. */
export function legalRoute(slug: string): string {
  return rootRoute(legalHref(slug));
}

// The official mention is « Accessibilité : totalement / partiellement /
// non conforme » (RGAA, décret 2019-768 art. 5).
const STATUS_LABELS: Record<string, string> = {
  non_compliant: "non conforme",
  partially_compliant: "partiellement conforme",
  compliant: "totalement conforme",
};

/** The French label of a published status, or null when none is published. */
export function accessibilityLabel(a: LegalAccessibility | undefined): string | null {
  const status = a?.status;
  if (!status) return null;
  return STATUS_LABELS[status] ?? null;
}

/** The whole footer mention, e.g. « Accessibilité : partiellement conforme
 *  (72 %) », or null when no status is published. */
export function accessibilityMention(a: LegalAccessibility | undefined): string | null {
  const label = accessibilityLabel(a);
  if (!label) return null;
  const rate = a?.rate?.trim();
  return rate ? `Accessibilité : ${label} (${rate} %)` : `Accessibilité : ${label}`;
}

/** Reads the legal block of the loaded server info; null until it loads, or
 *  on a server that predates the surface. */
export function useLegalInfo(): LegalInfo | null {
  return useServerInfoStore((s) => s.info?.legal ?? null);
}

/** True when the deployment published at least one thing: a document, a
 *  contact or an accessibility status. */
export function legalHasAnything(info: LegalInfo | null): boolean {
  if (!info) return false;
  return (
    info.documents.length > 0 ||
    Boolean(info.contact_email) ||
    accessibilityLabel(info.accessibility) !== null
  );
}
