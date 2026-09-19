// Pure helpers for the Usage-caps console, extracted from the page component
// so they can be unit-tested and shared without tripping react-refresh's
// "component files should only export components" rule.

import type { UsageCapsView } from "@/api/adminSettings";

export type WindowField = "five_hour_pct" | "week_pct";

// parseWindow validates a text field into the value a PUT carries: null means
// "clear to env" (a blank field), a number is the override. Returns an error
// string on an out-of-range or non-integer entry.
export function parseWindow(raw: string): { value: number | null; error?: string } {
  const t = raw.trim();
  if (t === "") return { value: null };
  const n = Number(t);
  if (!Number.isInteger(n) || n < 0 || n > 100) {
    return {
      value: null,
      error: "must be a whole number 0–100 (or blank to inherit the env default)",
    };
  }
  return { value: n };
}

// storedOf reads a window's stored override off the record (null when none, or
// when that specific field is absent — an inherit, never 0).
export function storedOf(view: UsageCapsView | undefined, field: WindowField): number | null {
  const rec = view?.record;
  if (!rec) return null;
  const v = rec[field];
  return typeof v === "number" ? v : null;
}
