// Pure helper for the platform-credentials console.

// sameSet reports whether two id lists hold the same members, order-
// independent. Used to decide whether an allow-list changed so the PUT omits
// it when it did not (merge semantics: an absent field is left untouched).
export function sameSet(a: string[], b: string[]): boolean {
  if (a.length !== b.length) return false;
  const sb = new Set(b);
  return a.every((x) => sb.has(x));
}
