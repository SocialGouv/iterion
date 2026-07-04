import { useState } from "react";

import { Button, Dialog, Input } from "@/components/ui";
import { desktop } from "@/lib/desktopBridge";

interface Props {
  open: boolean;
  onClose: () => void;
  // Called after a successful connect so the switcher can close + refresh.
  onConnected: () => void;
}

// CloudConnectModal is the desktop shell's "Connect to Cloud…" flow. It
// registers a remote iterion instance and logs in with a password; the Go
// side (ConnectCloud) validates the URL via /api/server/info, performs a
// native login, stores the refresh token in the OS keychain, and points the
// authenticating proxy at the remote. SSO buttons arrive in Phase 2.
export default function CloudConnectModal({ open, onClose, onConnected }: Props) {
  const [url, setUrl] = useState("");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const reset = () => {
    setUrl("");
    setEmail("");
    setPassword("");
    setError(null);
    setBusy(false);
  };

  const canSubmit = url.trim() !== "" && email.trim() !== "" && password !== "" && !busy;

  const onSubmit = async () => {
    if (!canSubmit) return;
    setBusy(true);
    setError(null);
    try {
      await desktop.connectCloud(url.trim(), email.trim(), password);
      reset();
      onConnected();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setBusy(false);
    }
  };

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        if (!o) {
          reset();
          onClose();
        }
      }}
      title="Connect to Cloud"
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
          Pilot a remote iterion cloud from the desktop app. Your session token
          is stored securely in the OS keychain — never in a config file.
        </p>
        <label className="flex flex-col gap-1 text-xs font-medium">
          Cloud URL
          <Input
            autoFocus
            placeholder="https://cloud.example.com"
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            size="md"
            disabled={busy}
          />
        </label>
        <label className="flex flex-col gap-1 text-xs font-medium">
          Email
          <Input
            type="email"
            placeholder="you@example.com"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            size="md"
            disabled={busy}
          />
        </label>
        <label className="flex flex-col gap-1 text-xs font-medium">
          Password
          <Input
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
            Cancel
          </Button>
          <Button type="submit" variant="primary" size="sm" disabled={!canSubmit}>
            {busy ? "Connecting…" : "Connect"}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
