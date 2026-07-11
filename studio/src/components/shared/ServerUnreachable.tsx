import { useEffect, useRef, useState } from "react";
import { Button } from "@/components/ui/Button";
import { Spinner } from "@/components/ui/Spinner";

// Full-viewport screen for the "server unreachable" auth state: the
// public /server/info probe failed on a network error or 5xx, so the
// backend itself is down — showing the sign-in form here would send a
// local-mode operator chasing credentials that don't exist. Retries
// the bootstrap automatically; the button is for the impatient.
const RETRY_INTERVAL_MS = 3000;

export default function ServerUnreachable({ onRetry }: { onRetry: () => Promise<void> }) {
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
    const t = setInterval(() => void attempt(), RETRY_INTERVAL_MS);
    return () => clearInterval(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <div className="h-screen flex flex-col items-center justify-center gap-4 bg-surface-0 text-fg-default px-6">
      <div className="text-3xl" aria-hidden>
        ⚡
      </div>
      <h1 className="text-lg font-semibold">Can&apos;t reach the iterion server</h1>
      <p className="text-sm text-fg-muted text-center max-w-md">
        The studio backend isn&apos;t responding. It may be restarting, stopped, or
        unreachable from this machine. Reconnecting automatically…
      </p>
      <div className="flex items-center gap-3">
        <Button variant="secondary" size="sm" onClick={() => void attempt()} disabled={retrying}>
          {retrying ? (
            <span className="inline-flex items-center gap-2">
              <Spinner size="xs" /> Retrying…
            </span>
          ) : (
            "Retry now"
          )}
        </Button>
      </div>
    </div>
  );
}
