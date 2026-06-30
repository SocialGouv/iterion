import { errorMessage } from "@/lib/errorHints";
import { useEffect, useState } from "react";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Button } from "@/components/ui/Button";
import { Input } from "@/components/ui/Input";
import { useLocation } from "wouter";

import { ApiError, completePendingPasswordChange } from "@/api/auth";
import { useAuth } from "@/auth/AuthContext";

// ForcedPasswordChange completes the pending_password_change flow for an
// account (typically the bootstrapped super-admin) whose login was
// rejected by /api/auth/login with HTTP 403 "password change required".
// The Login view navigates here carrying the email + the rejected
// password as query params so the user only types the new password.
export default function ForcedPasswordChange() {
  const { reloadIdentity } = useAuth();
  const [, navigate] = useLocation();

  const [email, setEmail] = useState("");
  const [currentPassword, setCurrentPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  // Pull email + temp from the URL so the operator's bootstrap mail
  // (or the Login view's redirect) just works. The history entry is
  // replaced immediately to avoid leaving the temp password in the
  // browser's URL bar / back stack.
  useEffect(() => {
    const u = new URL(window.location.href);
    const e = u.searchParams.get("email") ?? "";
    const p = u.searchParams.get("temp") ?? "";
    if (e) setEmail(e);
    if (p) setCurrentPassword(p);
    if (e || p) {
      const clean = window.location.pathname;
      window.history.replaceState({}, "", clean);
    }
  }, []);

  const submit = async (ev: React.FormEvent) => {
    ev.preventDefault();
    setErr(null);
    if (newPassword.length < 8) {
      setErr("New password must be at least 8 characters.");
      return;
    }
    if (newPassword !== confirmPassword) {
      setErr("The two new-password fields don't match.");
      return;
    }
    setBusy(true);
    try {
      await completePendingPasswordChange(email, currentPassword, newPassword);
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
        <h1 className="text-headline font-semibold mb-2">Choose a new password</h1>
        <p className="text-sm text-fg-muted mb-6">
          Your account was created with a temporary password. Set a new one to finish signing in.
        </p>
        <form onSubmit={submit} className="space-y-3" data-testid="forced-password-change-form">
          <div>
            <label htmlFor="forced-email" className="sr-only">
              Email
            </label>
            <Input
              size="md"
              type="email"
              id="forced-email"
              placeholder="Email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              autoComplete="email"
              required
            />
          </div>
          <div>
            <label htmlFor="forced-current-password" className="sr-only">
              Current (temporary) password
            </label>
            <Input
              size="md"
              type="password"
              id="forced-current-password"
              placeholder="Current (temporary) password"
              value={currentPassword}
              onChange={(e) => setCurrentPassword(e.target.value)}
              autoComplete="current-password"
              required
            />
          </div>
          <div>
            <label htmlFor="forced-new-password" className="sr-only">
              New password
            </label>
            <Input
              size="md"
              type="password"
              id="forced-new-password"
              placeholder="New password (≥ 8 characters)"
              value={newPassword}
              onChange={(e) => setNewPassword(e.target.value)}
              autoComplete="new-password"
              minLength={8}
              required
            />
          </div>
          <div>
            <label htmlFor="forced-confirm-password" className="sr-only">
              Confirm new password
            </label>
            <Input
              size="md"
              type="password"
              id="forced-confirm-password"
              placeholder="Confirm new password"
              value={confirmPassword}
              onChange={(e) => setConfirmPassword(e.target.value)}
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
            {busy ? "Working…" : "Set new password & sign in"}
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
