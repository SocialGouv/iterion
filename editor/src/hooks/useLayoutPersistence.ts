import { useCallback, useRef, useState } from "react";

import type { Layout } from "react-resizable-panels";

// useLayoutPersistence persists a Group's layout (a map of panel id →
// flexGrow) in localStorage so the user's panel sizes survive reloads.
// Returns the layout to feed into Group.defaultLayout and a stable
// onChange to wire to Group.onLayoutChanged. The hook is keyed by
// `key`; if storage is unreadable (private mode, quota), it falls back
// to `fallback`.
export function useLayoutPersistence(
  key: string,
  fallback: Layout,
): {
  layout: Layout;
  onChange: (next: Layout) => void;
} {
  const [layout] = useState<Layout>(() => readLayout(key) ?? fallback);
  // Throttle writes so a drag doesn't hammer localStorage.
  const writeTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const onChange = useCallback(
    (next: Layout) => {
      if (writeTimerRef.current) clearTimeout(writeTimerRef.current);
      writeTimerRef.current = setTimeout(() => {
        try {
          window.localStorage.setItem(key, JSON.stringify(next));
        } catch {
          // ignore — storage may be unavailable
        }
      }, 200);
    },
    [key],
  );
  return { layout, onChange };
}

function readLayout(key: string): Layout | null {
  try {
    const raw = window.localStorage.getItem(key);
    if (!raw) return null;
    const parsed = JSON.parse(raw) as unknown;
    if (!isLayout(parsed)) return null;
    return parsed;
  } catch {
    return null;
  }
}

function isLayout(v: unknown): v is Layout {
  if (!v || typeof v !== "object") return false;
  for (const k of Object.keys(v)) {
    const value = (v as Record<string, unknown>)[k];
    if (typeof value !== "number" || !Number.isFinite(value)) return false;
  }
  return true;
}
