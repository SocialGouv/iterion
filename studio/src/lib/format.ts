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

// Compact + full forms for the context-usage gauge shared between the
// canvas card (ContextUsageBar) and the node detail panel header.
// Returns null when the inputs are insufficient to render a meaningful
// gauge so callers can early-bail uniformly.
export interface ContextUsage {
  pct: number;
  label: string;
  title: string;
}

export function formatContextUsage(
  used: number | undefined,
  window: number | undefined,
): ContextUsage | null {
  if (!window || window <= 0 || used === undefined || used <= 0) return null;
  const pct = Math.min(100, (used / window) * 100);
  return {
    pct,
    label: `${formatTokens(used)}/${formatTokens(window)}`,
    title: `context: ${used.toLocaleString()} / ${window.toLocaleString()} tokens (${Math.round(pct)}%)`,
  };
}

// formatRelative renders an ISO timestamp as "5m ago" / "2h ago" /
// "3d ago" — or "in 5m" / "in 3d" for future instants (expiry dates,
// scheduled fires). Used by the run list and the commits panel; both
// want the same rounding behaviour so they stay in lockstep on screen.
export function formatRelative(iso: string): string {
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return iso;
  const delta = Date.now() - t;
  const future = delta < 0;
  const phrase = (n: number, unit: string) =>
    future ? `in ${n}${unit}` : `${n}${unit} ago`;
  const seconds = Math.round(Math.abs(delta) / 1000);
  if (seconds < 60) return phrase(seconds, "s");
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return phrase(minutes, "m");
  const hours = Math.round(minutes / 60);
  if (hours < 24) return phrase(hours, "h");
  const days = Math.round(hours / 24);
  return phrase(days, "d");
}

// Absolute timestamp formatters shared by every table/tooltip that shows
// a wall-clock instant. Locale is pinned to en-US so output is
// deterministic across dev hosts and CI ("Jul 18, 2026, 2:32 PM").
const dateTimeFormatter = new Intl.DateTimeFormat("en-US", {
  dateStyle: "medium",
  timeStyle: "short",
});
const dateFormatter = new Intl.DateTimeFormat("en-US", {
  dateStyle: "medium",
});
const timeFormatter = new Intl.DateTimeFormat("en-US", {
  timeStyle: "medium",
});

// formatDateTime renders an ISO timestamp as an absolute date + time,
// e.g. "Jul 18, 2026, 2:32 PM". Returns "—" for missing or unparsable
// input so callers can drop their own fallback branches.
export function formatDateTime(iso?: string | null): string {
  if (!iso) return "—";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "—";
  return dateTimeFormatter.format(t);
}

// formatDate renders an ISO timestamp as an absolute date only,
// e.g. "Jul 18, 2026". Same fallbacks as formatDateTime.
export function formatDate(iso?: string | null): string {
  if (!iso) return "—";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "—";
  return dateFormatter.format(t);
}

// formatTime renders an ISO timestamp as wall-clock time only,
// e.g. "2:32:05 PM". Same fallbacks as formatDateTime.
export function formatTime(iso?: string | null): string {
  if (!iso) return "—";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "—";
  return timeFormatter.format(t);
}

// formatDayHeader renders an ISO timestamp as a short weekday + date for
// day-group headers, e.g. "Sat, Jul 18" — appending the year only when
// it differs from the current one. Same fallbacks as formatDateTime.
export function formatDayHeader(iso?: string | null): string {
  if (!iso) return "—";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "—";
  const d = new Date(t);
  const opts: Intl.DateTimeFormatOptions = {
    weekday: "short",
    month: "short",
    day: "numeric",
  };
  if (d.getFullYear() !== new Date().getFullYear()) opts.year = "numeric";
  return new Intl.DateTimeFormat("en-US", opts).format(d);
}

// basename returns the trailing path segment after the last "/" or "\",
// ignoring a trailing separator. "/a/b" → "b", "/a/b/" → "b",
// "C:\\dev\\x" → "x", "group/project" → "project". Returns the input
// unchanged when it has no separator. Handles both POSIX git paths and
// host filesystem paths (incl. Windows for the desktop app).
export function basename(path: string): string {
  const trimmed = path.replace(/[/\\]+$/, "");
  const i = Math.max(trimmed.lastIndexOf("/"), trimmed.lastIndexOf("\\"));
  return i < 0 ? trimmed : trimmed.slice(i + 1);
}

export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`;
  return `${(n / (1024 * 1024)).toFixed(2)} MiB`;
}
