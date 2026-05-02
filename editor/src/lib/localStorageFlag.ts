// Tiny wrappers around localStorage for boolean UI flags. Centralized so
// every collapse/expand toggle in the app handles the "storage is
// unavailable" edge case the same way (private mode, quota errors, SSR).

export function readBooleanFlag(key: string, fallback = false): boolean {
  try {
    const raw = window.localStorage.getItem(key);
    if (raw === null) return fallback;
    return raw === "1";
  } catch {
    return fallback;
  }
}

export function writeBooleanFlag(key: string, value: boolean): void {
  try {
    window.localStorage.setItem(key, value ? "1" : "0");
  } catch {
    // storage may be unavailable
  }
}
