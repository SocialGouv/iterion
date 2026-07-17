import { errorMessage } from "@/lib/errorHints";
import { useState } from "react";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Button } from "@/components/ui/Button";
import { Input } from "@/components/ui/Input";
import { useLocation } from "wouter";

import { requestPasswordReset } from "@/api/auth";
import { useServerInfoStore } from "@/store/serverInfo";

// ForgotPassword fires the reset-mail request. The server is
// anti-enumeration (always 200), so the view shows the same generic
// confirmation regardless of whether the email matched an account.
// When the deployment has no outbound email, self-reset can't work —
// the view then explains the admin-assisted path instead of offering
// a form that silently goes nowhere.
export default function ForgotPassword() {
  const [, navigate] = useLocation();
  const emailEnabled = useServerInfoStore((s) => s.info?.email_enabled === true);
  const [email, setEmail] = useState("");
  const [busy, setBusy] = useState(false);
  const [sent, setSent] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const submit = async (ev: React.FormEvent) => {
    ev.preventDefault();
    setBusy(true);
    setErr(null);
    try {
      await requestPasswordReset(email);
      setSent(true);
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  if (!emailEnabled) {
    return (
      <div className="min-h-screen flex items-center justify-center bg-surface-0 text-fg-default px-4">
        <div className="w-full max-w-md bg-surface-1 border border-border-subtle rounded-[var(--radius-lg)] p-8 shadow-[var(--shadow-lg)] space-y-4">
          <h1 className="text-headline font-semibold">Forgot your password?</h1>
          <p className="text-sm text-fg-muted">
            Self-service reset is unavailable — this deployment doesn&apos;t
            send email. Ask a workspace administrator to reset your access
            (they can issue you a fresh invitation or a temporary password).
          </p>
          <Button variant="ghost" size="sm" onClick={() => navigate("/login")}>
            Back to sign-in
          </Button>
        </div>
      </div>
    );
  }

  if (sent) {
    return (
      <div className="min-h-screen flex items-center justify-center bg-surface-0 text-fg-default px-4">
        <div className="w-full max-w-md bg-surface-1 border border-border-subtle rounded-[var(--radius-lg)] p-8 shadow-[var(--shadow-lg)] space-y-4">
          <h1 className="text-headline font-semibold">Check your email</h1>
          <p className="text-sm text-fg-muted">
            If we have an account for that email address, we sent a password-reset link.
            The link expires shortly — open it in the same browser to finish resetting.
          </p>
          <Button
            variant="ghost"
            size="sm"
            onClick={() => navigate("/login")}
          >
            Back to sign-in
          </Button>
        </div>
      </div>
    );
  }

  return (
    <div className="min-h-screen flex items-center justify-center bg-surface-0 text-fg-default px-4">
      <div className="w-full max-w-md bg-surface-1 border border-border-subtle rounded-[var(--radius-lg)] p-8 shadow-[var(--shadow-lg)]">
        <h1 className="text-headline font-semibold mb-2">Forgot your password?</h1>
        <p className="text-sm text-fg-muted mb-6">
          Enter your email and we'll send a one-time link to reset it.
        </p>
        <form onSubmit={submit} className="space-y-3">
          <div>
            <label htmlFor="forgot-email" className="sr-only">
              Email
            </label>
            <Input
              size="md"
              type="email"
              id="forgot-email"
              placeholder="Email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
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
            {busy ? "Sending…" : "Send reset link"}
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
