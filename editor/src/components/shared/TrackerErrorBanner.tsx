// TrackerErrorBanner surfaces a sticky banner when the conductor's
// last tracker.ListCandidates call failed. Rendered identically on
// the Board view and the Conductor dashboard so the two surfaces
// don't drift apart when the upstream error message gets richer.
//
// Recognised substrings get a more specific guidance line so common
// failure modes (expired GitHub token, Forgejo 401, unreachable
// host) read at a glance.
export interface TrackerErrorBannerProps {
  tracker: string;
  message: string;
}

export default function TrackerErrorBanner({
  tracker,
  message,
}: TrackerErrorBannerProps) {
  const guidance = trackerErrorGuidance(tracker, message);
  return (
    <div className="bg-amber-500/10 border-b border-amber-500/40 px-4 py-2 text-xs text-amber-200 flex items-start gap-2">
      <span className="font-medium shrink-0">Tracker error:</span>
      <div className="flex-1 min-w-0">
        <div className="font-mono break-words">{message}</div>
        {guidance && <div className="mt-0.5 text-amber-200/80">{guidance}</div>}
      </div>
      <span className="text-amber-200/60 shrink-0">({tracker})</span>
    </div>
  );
}

function trackerErrorGuidance(tracker: string, err: string): string | null {
  const e = err.toLowerCase();
  if (e.includes("401") || e.includes("bad credentials") || e.includes("unauthorized")) {
    if (tracker === "github") {
      return "GitHub credentials rejected — the token in conductor.yaml is missing, expired, or lacks `issues:read` / `issues:write`. Regenerate and reload the conductor.";
    }
    if (tracker === "forgejo") {
      return "Forgejo credentials rejected — the personal access token is missing, expired, or lacks the issue scope. Regenerate and reload.";
    }
    return "Authentication rejected by the tracker. Check the configured token.";
  }
  if (e.includes("403") || e.includes("forbidden") || e.includes("rate limit")) {
    return "Tracker is rate-limiting or refusing the request. Wait a few minutes; if it persists, swap to a higher-scope token.";
  }
  if (
    e.includes("no such host") ||
    e.includes("connection refused") ||
    e.includes("i/o timeout") ||
    e.includes("dial tcp")
  ) {
    return "Cannot reach the tracker host. Check network connectivity and the configured base URL.";
  }
  return null;
}
