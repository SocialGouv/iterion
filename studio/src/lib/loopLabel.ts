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
 * A loop that lifts the budget guard (`as name(3) guard: off`, #2677)
 * shows it: `loop:name(3) guard:off`. A loop that declares its work
 * signal (`sized_by "outputs.probe.pending"`) shows that too, truncated
 * like an expression bound: `loop:name(3) sized:outputs.probe.pending`.
 * Never interpolates undefined into the label.
 */
export function formatLoopBadge(loop: LoopClause): string {
  const guard = loop.guard ? ` guard:${loop.guard}` : "";
  const sized = loop.sized_by
    ? ` sized:${loop.sized_by.length > MAX_EXPR_LEN ? `${loop.sized_by.slice(0, MAX_EXPR_LEN)}…` : loop.sized_by}`
    : "";
  const suffix = guard + sized;
  if (loop.unbounded) {
    const base =
      loop.fuel_cap && loop.fuel_cap > 0
        ? `loop:${loop.name}(unbounded ${loop.fuel_cap})`
        : `loop:${loop.name}(unbounded)`;
    return base + suffix;
  }
  if (typeof loop.max_iterations === "number" && loop.max_iterations > 0) {
    return `loop:${loop.name}(${loop.max_iterations})` + suffix;
  }
  const expr = loop.max_iterations_expr;
  if (expr) {
    const shown = expr.length > MAX_EXPR_LEN ? `${expr.slice(0, MAX_EXPR_LEN)}…` : expr;
    return `loop:${loop.name}(${shown})` + suffix;
  }
  return `loop:${loop.name}` + suffix;
}
