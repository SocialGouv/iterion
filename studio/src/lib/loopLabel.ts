import type { LoopClause } from "@/api/types";

// Longest expression bound rendered verbatim before truncating with an
// ellipsis — matches the `when.expr` truncation used on canvas edges.
const MAX_EXPR_LEN = 24;

/**
 * Formats the `loop:` badge shown on canvas edge labels and node-detail
 * sub-nodes. The AST JSON omits the bound for the unbounded form
 * (`as name(unbounded)`) and carries a template string for the
 * expression form (`as name("{{...}}")`), so the bound can be a number,
 * a string, or absent:
 *   - literal bound     → `loop:name(5)`
 *   - expression bound  → `loop:name({{vars.max}})` (truncated)
 *   - unbounded         → `loop:name(unbounded)` / `loop:name(unbounded 40)`
 *   - no bound          → `loop:name`
 * Never interpolates undefined into the label.
 */
export function formatLoopBadge(loop: LoopClause): string {
  if (loop.unbounded) {
    return loop.fuel_cap && loop.fuel_cap > 0
      ? `loop:${loop.name}(unbounded ${loop.fuel_cap})`
      : `loop:${loop.name}(unbounded)`;
  }
  if (typeof loop.max_iterations === "number" && loop.max_iterations > 0) {
    return `loop:${loop.name}(${loop.max_iterations})`;
  }
  const expr = loop.max_iterations_expr;
  if (expr) {
    const shown = expr.length > MAX_EXPR_LEN ? `${expr.slice(0, MAX_EXPR_LEN)}…` : expr;
    return `loop:${loop.name}(${shown})`;
  }
  return `loop:${loop.name}`;
}
