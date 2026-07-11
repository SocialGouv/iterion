import { useEffect, useRef, useState } from "react";
import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";

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
    <div className="h-screen bg-surface-0">
      <EmptyState
        icon={<span className="text-2xl">⚡</span>}
        title="Can't reach the iterion server"
        message="The studio backend isn't responding. It may be restarting, stopped, or unreachable from this machine. Reconnecting automatically…"
        action={
          <Button
            variant="secondary"
            size="sm"
            loading={retrying}
            onClick={() => void attempt()}
          >
            {retrying ? "Retrying…" : "Retry now"}
          </Button>
        }
      />
    </div>
  );
}
