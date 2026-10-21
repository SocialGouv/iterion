// TrackerErrorBanner surfaces a sticky banner when the dispatcher's
// last tracker.ListCandidates call failed. Rendered identically on
// the Board view and the Dispatcher dashboard so the two surfaces
// don't drift apart when the upstream error message gets richer.
//
// Recognised substrings get a more specific guidance line so common
// failure modes (expired GitHub token, Forgejo 401, unreachable
// host) read at a glance.
import { InlineBanner } from "@/components/ui/InlineBanner";

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
    <InlineBanner tone="warning" title="Tracker error" suffix={`(${tracker})`}>
      <div className="font-mono break-words">{message}</div>
      {guidance && <div className="mt-0.5">{guidance}</div>}
    </InlineBanner>
  );
}

function trackerErrorGuidance(tracker: string, err: string): string | null {
  const e = err.toLowerCase();
  if (e.includes("401") || e.includes("bad credentials") || e.includes("unauthorized")) {
    if (tracker === "github") {
      return "GitHub credentials rejected — the token in dispatcher.yaml is missing, expired, or lacks `issues:read` / `issues:write`. Regenerate and reload the dispatcher.";
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
