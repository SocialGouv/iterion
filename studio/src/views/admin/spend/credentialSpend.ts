// Pure helpers for the Credential-spend console.
import type { AdminCredentialUsageQuery } from "@/api/adminCredUsage";

export const SPEND_TIERS = ["platform", "team", "org", "pool"] as const;
export type SpendTier = (typeof SPEND_TIERS)[number];

// A YYYY-MM string is well-formed (the API rejects a malformed one). Empty is
// allowed — it means "current month".
export function isValidMonth(raw: string): boolean {
  const t = raw.trim();
  if (t === "") return true;
  return /^\d{4}-(0[1-9]|1[0-2])$/.test(t);
}

// buildQuery turns the form state into the API query. fingerprint and repo are
// mutually exclusive server-side; a non-empty fingerprint wins and repo is
// dropped, so the UI never sends the pair the API would 400 on. tier is only
// meaningful when neither is set.
export function buildQuery(form: {
  tier: SpendTier;
  month: string;
  fingerprint: string;
  repo: string;
}): AdminCredentialUsageQuery {
  const fingerprint = form.fingerprint.trim();
  const repo = form.repo.trim();
  const month = form.month.trim();
  const q: AdminCredentialUsageQuery = {};
  if (month) q.month = month;
  if (fingerprint) {
    q.fingerprint = fingerprint;
  } else if (repo) {
    q.repo = repo;
  } else {
    q.tier = form.tier;
  }
  return q;
}

// formatUSD renders a dollar amount to cents. A subscription's estimated cost
// and a metered invoice are the SAME format but different nature — the caller
// must label them, never sum them.
export function formatUSD(n: number): string {
  return `$${n.toFixed(2)}`;
}

// formatTokens renders the three counters the API carries apart, per the
// server's own contract (pkg/credusage/credusage.go): input/output are
// DIRECTIONAL and carry a split that was actually observed, aggregate_tokens
// carries the unsplittable total a CLI delegate reports (#992), and "a total
// is the sum of the three".
//
// A row is the merge of every repo/backend of one credential-month, so a
// credential served by BOTH a split-reporting backend and a CLI delegate
// carries a split AND an aggregate: show them together rather than picking a
// side and being wrong in silence. Zero everywhere means "not observed", never
// "none spent", so it says so instead of rendering a false "0 in / 0 out".
export function formatTokens(row: {
  input_tokens: number;
  output_tokens: number;
  aggregate_tokens: number;
}): string {
  const split = `${row.input_tokens.toLocaleString()} in / ${row.output_tokens.toLocaleString()} out`;
  const aggregate = `${row.aggregate_tokens.toLocaleString()} (aggregate)`;
  const hasSplit = row.input_tokens > 0 || row.output_tokens > 0;
  if (row.aggregate_tokens > 0) return hasSplit ? `${split} + ${aggregate}` : aggregate;
  return hasSplit ? split : "not reported";
}
