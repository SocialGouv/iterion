import { useEffect, useState } from "react";

import { Button, Dialog, Input } from "@/components/ui";
import { desktop } from "@/lib/desktopBridge";

interface Props {
  // Non-null opens the modal for that cloud connection id (the payload of the
  // cloud:auth-expired event). Null keeps it closed.
  connId: string | null;
  onClose: () => void;
}

// CloudReloginModal recovers an expired cloud session. When the desktop emits
// cloud:auth-expired (silent refresh was rejected / the token was revoked),
// the shell opens this so the operator re-authenticates. loginCloud re-seeds
// the token jar AND restarts the background refresh loop, so the connection is
// fully live again — a plain proxy cookie-harvest would re-seed the jar but
// leave the refresh loop stopped.
export default function CloudReloginModal({ connId, onClose }: Props) {
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Prefill the email from the stored connection when the modal opens.
  useEffect(() => {
    if (!connId) return;
    let cancelled = false;
    void desktop
      .listConnections()
      .then((conns) => {
        if (cancelled) return;
        const c = conns.find((p) => p.id === connId);
        if (c?.cloud_email) setEmail(c.cloud_email);
      })
      .catch(() => {});
    return () => {
      cancelled = true;
    };
  }, [connId]);

  const reset = () => {
    setPassword("");
    setError(null);
    setBusy(false);
  };

  const canSubmit = !!connId && email.trim() !== "" && password !== "" && !busy;

  const onSubmit = async () => {
    if (!canSubmit || !connId) return;
    setBusy(true);
    setError(null);
    try {
      await desktop.loginCloud(connId, email.trim(), password);
      reset();
      onClose();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setBusy(false);
    }
  };

  return (
    <Dialog
      open={!!connId}
      onOpenChange={(o) => {
        if (!o) {
          reset();
          onClose();
        }
      }}
      title="Cloud session expired"
      widthClass="max-w-md"
    >
      <form
        className="flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault();
          void onSubmit();
        }}
      >
        <p className="text-xs text-fg-subtle">
          Your cloud session ended. Sign in again to keep piloting this
          connection.
        </p>
        <label className="flex flex-col gap-1 text-xs font-medium">
          Email
          <Input
            type="email"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            size="md"
            disabled={busy}
          />
        </label>
        <label className="flex flex-col gap-1 text-xs font-medium">
          Password
          <Input
            autoFocus
            type="password"
            placeholder="••••••••"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            size="md"
            disabled={busy}
          />
        </label>
        {error && (
          <div
            className="text-xs text-danger-fg bg-danger-soft border border-danger/40 rounded px-2 py-1.5"
            role="alert"
          >
            {error}
          </div>
        )}
        <div className="flex justify-end gap-2 pt-1">
          <Button
            type="button"
            variant="ghost"
            size="sm"
            onClick={() => {
              reset();
              onClose();
            }}
            disabled={busy}
          >
            Dismiss
          </Button>
          <Button type="submit" variant="primary" size="sm" disabled={!canSubmit}>
            {busy ? "Signing in…" : "Sign in"}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
