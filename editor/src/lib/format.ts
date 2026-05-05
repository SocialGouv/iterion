// formatMs renders a millisecond value as a compact human-readable
// duration: 750ms → "750ms", 12s → "12s", 1m23s → "1m23s", 1h05m12s.
// Used by the run console for both per-execution and per-run timing.
export function formatMs(ms: number): string {
  if (ms < 0) ms = 0;
  if (ms < 1000) return `${Math.round(ms)}ms`;
  const totalSec = Math.floor(ms / 1000);
  const h = Math.floor(totalSec / 3600);
  const m = Math.floor((totalSec % 3600) / 60);
  const s = totalSec % 60;
  if (h > 0)
    return `${h}h${m.toString().padStart(2, "0")}m${s.toString().padStart(2, "0")}s`;
  if (m > 0) return `${m}m${s.toString().padStart(2, "0")}s`;
  return `${s}s`;
}

// formatDurationBetween computes an ISO-string duration. Returns null
// when the input is malformed; falls back to "now" when end is omitted
// (live ticker case).
export function formatDurationBetween(
  start?: string,
  end?: string,
): string | null {
  if (!start) return null;
  const startMs = new Date(start).getTime();
  if (!Number.isFinite(startMs)) return null;
  const endMs = end ? new Date(end).getTime() : Date.now();
  if (!Number.isFinite(endMs)) return null;
  return formatMs(endMs - startMs);
}

export function formatCost(usd: number): string {
  if (usd < 0.0001) return "$0";
  if (usd < 0.01) return `$${usd.toFixed(4)}`;
  if (usd < 1) return `$${usd.toFixed(3)}`;
  return `$${usd.toFixed(2)}`;
}

export function formatTokens(n: number): string {
  if (n < 1000) return String(n);
  if (n < 1_000_000) return `${(n / 1000).toFixed(1)}k`;
  return `${(n / 1_000_000).toFixed(2)}M`;
}

// formatRelative renders an ISO timestamp as "5m ago" / "2h ago" /
// "3d ago". Used by the run list and the commits panel; both want the
// same rounding behaviour so they stay in lockstep on screen.
export function formatRelative(iso: string): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return iso;
  const delta = Date.now() - t;
  const seconds = Math.round(delta / 1000);
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.round(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.round(hours / 24);
  return `${days}d ago`;
}

export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`;
  return `${(n / (1024 * 1024)).toFixed(2)} MiB`;
}
