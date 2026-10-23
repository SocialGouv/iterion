import { useState } from "react";
import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";

// What a workspace pane shows when the desktop could not authenticate it
// because the SYSTEM KEYCHAIN is locked (503 keychain_locked): neither the
// server nor the account is at fault, and the way out is on this computer —
// unlock the login keyring, then retry. "The server could not load your
// account" sends the operator to the wrong place entirely.
//
// Nothing retries on its own, and that is the point: every keychain read is an
// OS unlock prompt, so an automatic cadence is a prompt the operator did not
// ask for, again and again. The button is the ask — and it tells the desktop
// to read now, whatever back-off it is on.
export default function KeychainLocked({ onRetry }: { onRetry: () => Promise<void> }) {
  const [retrying, setRetrying] = useState(false);
  const attempt = async () => {
    if (retrying) return;
    setRetrying(true);
    try {
      await onRetry();
    } finally {
      setRetrying(false);
    }
  };
  return (
    <div className="h-screen bg-surface-0">
      <EmptyState
        icon={<span className="text-2xl">🔒</span>}
        title="Your system keychain is locked"
        message="iterion keeps this connection's credentials in your operating system's keychain, and it is locked right now — so this pane cannot sign in. Unlock your login keyring (your desktop asks for your password), then retry. Nothing is retried on its own: each attempt asks you to unlock."
        action={
          <Button variant="secondary" size="sm" loading={retrying} onClick={() => void attempt()}>
            {retrying ? "Retrying…" : "I unlocked it — retry"}
          </Button>
        }
      />
    </div>
  );
}
