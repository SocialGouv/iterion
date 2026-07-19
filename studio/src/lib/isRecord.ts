// isRecord narrows an unknown value to a plain string-keyed object —
// the honest (runtime-checked) alternative to `as Record<string,
// unknown>` when unwrapping opaque wire payloads (event data extras,
// checkpoint fields behind index signatures, JSON.parse results).
export function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}
