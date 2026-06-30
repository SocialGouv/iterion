import { errorMessage } from "@/lib/errorHints";
import { useEffect, useState } from "react";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Button } from "@/components/ui/Button";
import { Input } from "@/components/ui/Input";
import { useLocation } from "wouter";

import { ApiError, confirmPasswordReset } from "@/api/auth";
import { useAuth } from "@/auth/AuthContext";

// ResetPassword consumes ?token=... from the reset email, sets the new
// password, and lets the freshly-issued cookies take over (the server
// renderAuthResponse path).
export default function ResetPassword() {
  const { reloadIdentity } = useAuth();
  const [, navigate] = useLocation();
  const [token, setToken] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    const u = new URL(window.location.href);
    const t = u.searchParams.get("token") ?? "";
    if (t) setToken(t);
  }, []);

  const submit = async (ev: React.FormEvent) => {
    ev.preventDefault();
    setErr(null);
    if (!token) {
      setErr("Missing reset token. Re-open the link from the email.");
      return;
    }
    if (password.length < 8) {
      setErr("Password must be at least 8 characters.");
      return;
    }
    if (password !== confirm) {
      setErr("The two password fields don't match.");
      return;
    }
    setBusy(true);
    try {
      await confirmPasswordReset(token, password);
      await reloadIdentity();
      navigate("/", { replace: true });
    } catch (e) {
      const msg = e instanceof ApiError ? e.message : errorMessage(e);
      setErr(msg);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="min-h-screen flex items-center justify-center bg-surface-0 text-fg-default px-4">
      <div className="w-full max-w-md bg-surface-1 border border-border-subtle rounded-[var(--radius-lg)] p-8 shadow-[var(--shadow-lg)]">
        <h1 className="text-headline font-semibold mb-2">Reset your password</h1>
        <p className="text-sm text-fg-muted mb-6">Pick a new password to finish.</p>
        <form onSubmit={submit} className="space-y-3">
          <div>
            <label htmlFor="reset-password" className="sr-only">
              New password
            </label>
            <Input
              size="md"
              type="password"
              id="reset-password"
              placeholder="New password (≥ 8 characters)"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete="new-password"
              minLength={8}
              required
            />
          </div>
          <div>
            <label htmlFor="reset-password-confirm" className="sr-only">
              Confirm new password
            </label>
            <Input
              size="md"
              type="password"
              id="reset-password-confirm"
              placeholder="Confirm new password"
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
              autoComplete="new-password"
              minLength={8}
              required
            />
          </div>
          {err && (
            <InlineBanner tone="danger" layout="inline">
              {err}
            </InlineBanner>
          )}
          <Button
            variant="primary"
            type="submit"
            loading={busy}
            className="w-full"
          >
            {busy ? "Working…" : "Reset password & sign in"}
          </Button>
        </form>
        <div className="mt-4 text-sm text-fg-muted text-center">
          <Button
            variant="ghost"
            size="sm"
            onClick={() => navigate("/login")}
          >
            Back to sign-in
          </Button>
        </div>
      </div>
    </div>
  );
}
