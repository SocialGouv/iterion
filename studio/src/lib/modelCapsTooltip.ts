import type { ModelCapabilities } from "@/api/client";

// Renders a model's capabilities as one caption line, e.g.
//   "1M context · 64K max out · $5.00 / $25.00 per M · aggregator"
//
// Every field is zero-means-UNKNOWN rather than zero-means-none: the spec
// aggregator zeroes any figure its publishers disagree on, so a zero is a
// routine answer. A segment whose value is unknown is therefore OMITTED,
// never printed as "0" — "$0.00 per M" would read as a free model.

// formatTokens renders a token count the way the CLI's model table does
// (1M, 200K, 4096), so the two surfaces describe the same model the same way.
export function formatTokens(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return "";
  if (n % 1_000_000 === 0) return `${n / 1_000_000}M`;
  if (n % 1_000 === 0) return `${n / 1_000}K`;
  return String(n);
}

// formatPricePair renders the per-million-token pair, or "price unknown".
//
// A HALF-published pair counts as unknown, matching the cost estimator's own
// rule: the two rates are published independently, and showing one beside a
// silent zero invites reading the missing half as free. The estimator refuses
// such a pair whole, so the caption must not imply a price the run would not
// be charged at.
export function formatPricePair(inputPerM: number, outputPerM: number): string {
  if (!(inputPerM > 0) || !(outputPerM > 0)) return "price unknown";
  return `$${inputPerM.toFixed(2)} / $${outputPerM.toFixed(2)} per M`;
}

export function modelCapsTooltip(caps: ModelCapabilities | null | undefined): string {
  if (!caps) return "";

  const context = formatTokens(caps.context_window);
  const maxOut = formatTokens(caps.max_output_tokens);
  const hasPrice = caps.input_cost_per_m > 0 && caps.output_cost_per_m > 0;
  // Nothing is known — for a model no source carries at all. Rendering
  // "price unknown · curated" here would put a line under the picker that
  // says only that iterion has nothing to say.
  if (!context && !maxOut && !hasPrice) return "";

  const segments: string[] = [];
  if (context) segments.push(`${context} context`);
  if (maxOut) segments.push(`${maxOut} max out`);
  segments.push(formatPricePair(caps.input_cost_per_m, caps.output_cost_per_m));

  // The source is always shown: it is what tells the reader whether the
  // numbers above came from the published spec table or from iterion's
  // curated fallback, which carries no price at all.
  segments.push(caps.source);

  return segments.join(" · ");
}
