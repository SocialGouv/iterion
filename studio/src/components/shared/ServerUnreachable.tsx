import { useEffect, useRef, useState } from "react";
import { IDENTITY_RETRY_INTERVAL_MS } from "@/auth/AuthContext";
import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";

// Full-viewport retry screen for an auth state the server could not settle:
// "unreachable" (the public /server/info probe failed on a network error or
// 5xx, so the backend itself is down — showing the sign-in form here would
// send a local-mode operator chasing credentials that don't exist) and
// "identity_unavailable" (the session is valid but the server could not say
// who it belongs to — showing the sign-in form, or an account that belongs
// to nothing, would both be false). Retries the bootstrap automatically;
// the button is for the impatient. title and message name the state;
// secondaryAction is the way out of it the state offers (signing out). The
// cadence is the auth module's own (IDENTITY_RETRY_INTERVAL_MS): this screen's
// retry and the background re-read of a stale tree ask the same server for the
// same thing.

export default function ServerUnreachable({
  onRetry,
  title = "Can't reach the iterion server",
  message = "The studio backend isn't responding. It may be restarting, stopped, or unreachable from this machine. Reconnecting automatically…",
  secondaryAction,
}: {
  onRetry: () => Promise<void>;
  title?: string;
  message?: string;
  secondaryAction?: { label: string; onClick: () => void | Promise<void> };
}) {
  const [retrying, setRetrying] = useState(false);
  const inFlight = useRef(false);

  const attempt = async () => {
    if (inFlight.current) return;
    inFlight.current = true;
    setRetrying(true);
    try {
      await onRetry();
    } finally {
      inFlight.current = false;
      setRetrying(false);
    }
  };

  useEffect(() => {
    const t = setInterval(() => void attempt(), IDENTITY_RETRY_INTERVAL_MS);
    return () => clearInterval(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <div className="h-screen bg-surface-0">
      <EmptyState
        icon={<span className="text-2xl">⚡</span>}
        title={title}
        message={message}
        action={
          <div className="flex items-center gap-2">
            <Button
              variant="secondary"
              size="sm"
              loading={retrying}
              onClick={() => void attempt()}
            >
              {retrying ? "Retrying…" : "Retry now"}
            </Button>
            {secondaryAction && (
              <Button variant="ghost" size="sm" onClick={() => void secondaryAction.onClick()}>
                {secondaryAction.label}
              </Button>
            )}
          </div>
        }
      />
    </div>
  );
}
