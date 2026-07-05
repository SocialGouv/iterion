import { useEffect, useState } from "react";

import { Button, Dialog, Input } from "@/components/ui";
import { desktop, type CloudProvider } from "@/lib/desktopBridge";

interface Props {
  open: boolean;
  onClose: () => void;
  // Called after a successful connect so the switcher can close + refresh.
  onConnected: () => void;
}

// CloudConnectModal is the desktop shell's "Connect to Cloud…" flow. Enter a
// cloud URL and its OAuth providers are auto-discovered: "Continue with
// <provider>" opens the cloud's authorize page in the system browser and
// completes over a single-use loopback ticket (ConnectCloudSSO) — the primary,
// password-less path. Email + password (ConnectCloud) is the fallback, or the
// only option for a cloud with no SSO. Either way the Go side validates the URL
// via /api/server/info, stores the refresh token in the OS keychain, and points
// the authenticating proxy at the remote.
export default function CloudConnectModal({ open, onClose, onConnected }: Props) {
  const [url, setUrl] = useState("");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [providers, setProviders] = useState<CloudProvider[] | null>(null);
  const [showPassword, setShowPassword] = useState(false);

  const reset = () => {
    setUrl("");
    setEmail("");
    setPassword("");
    setError(null);
    setBusy(false);
    setProviders(null);
    setShowPassword(false);
  };

  // Auto-discover the cloud's OAuth providers once a plausible URL is typed, so
  // the "Continue with <provider>" (browser OAuth) buttons appear without an
  // extra click — OAuth is the primary, lowest-friction connect path. Debounced
  // and best-effort: a failed probe just leaves the SSO buttons hidden and the
  // email/password fallback available. `email` is passed so a cloud with
  // per-org (tenant) IdPs can resolve the right one.
  useEffect(() => {
    const u = url.trim();
    if (!open || !/^https?:\/\/.+/.test(u)) {
      setProviders(null);
      return;
    }
    let cancelled = false;
    const t = setTimeout(() => {
      desktop
        .listCloudProviders(u, email.trim())
        .then((res) => {
          if (!cancelled) setProviders(res.providers ?? []);
        })
        .catch(() => {
          if (!cancelled) setProviders(null);
        });
    }, 600);
    return () => {
      cancelled = true;
      clearTimeout(t);
    };
  }, [url, email, open]);

  const onSSO = async (provider: string) => {
    setBusy(true);
    setError(null);
    try {
      await desktop.connectCloudSSO(url.trim(), provider);
      reset();
      onConnected();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setBusy(false);
    }
  };

  const hasProviders = (providers?.length ?? 0) > 0;
  // Password fallback is shown when the user asks for it, or when the cloud has
  // no OAuth providers to offer (then it's the only way in).
  const showPasswordForm = showPassword || !hasProviders;
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

        {error && (
          <div
            className="text-xs text-danger-fg bg-danger-soft border border-danger/40 rounded px-2 py-1.5"
            role="alert"
          >
            {error}
          </div>
        )}

        {/* OAuth is the primary path: discovered from the URL, it opens the
            cloud's authorize page in your browser and completes over a loopback
            ticket — no password typed into the desktop. */}
        {hasProviders && (
          <div className="flex flex-col gap-1.5">
            {providers!.map((p) => (
              <Button
                key={p.name}
                type="button"
                variant="primary"
                size="md"
                onClick={() => void onSSO(p.name)}
                disabled={busy}
              >
                {busy ? "Opening browser…" : `Continue with ${p.display || p.name}`}
              </Button>
            ))}
          </div>
        )}

        {/* Email + password: the fallback (or the only option when a cloud has
            no SSO). Shown by default when no providers were discovered. */}
        {showPasswordForm ? (
          <>
            {hasProviders && (
              <div className="flex items-center gap-2">
                <div className="h-px flex-1 bg-border-default" />
                <span className="text-caption uppercase tracking-wider text-fg-subtle">
                  or with email
                </span>
                <div className="h-px flex-1 bg-border-default" />
              </div>
            )}
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
          </>
        ) : (
          <div className="flex items-center justify-between pt-1">
            <button
              type="button"
              className="text-xs text-fg-subtle hover:text-fg-default underline underline-offset-2"
              onClick={() => setShowPassword(true)}
              disabled={busy}
            >
              Sign in with email &amp; password instead
            </button>
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
          </div>
        )}
      </form>
    </Dialog>
  );
}
