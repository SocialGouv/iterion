// The one place the studio decides WHICH model a picker is actually talking
// about. Every model picker needs this and each was deriving it differently.
//
// The launch picker is the case that forces a shared helper: its input `value`
// is the OVERRIDE (empty in the common case), and the node's own model lives
// only in the placeholder. Anything keyed on the input value alone therefore
// shows nothing precisely when the operator has changed nothing — the inherit
// path, which is most launches.
//
// Precedence: an explicit override, then the node's own model, then whatever
// that literal expands to. An unexpanded `${VAR}` is not a model id, so it is
// dropped rather than passed on to a lookup that could only miss.
export function effectiveModel(
  override: string | undefined,
  authored: string | undefined,
  resolved?: string | undefined,
): string {
  const chosen = override?.trim();
  if (chosen) return chosen;

  const literal = authored?.trim() ?? "";
  if (literal === "") return "";
  if (!literal.includes("$")) return literal;

  const expanded = resolved?.trim() ?? "";
  return expanded.includes("$") ? "" : expanded;
}
