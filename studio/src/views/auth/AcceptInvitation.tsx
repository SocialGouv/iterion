import { errorMessage } from "@/lib/errorHints";
import { useEffect, useState } from "react";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Button } from "@/components/ui/Button";
import { Spinner } from "@/components/ui/Spinner";
import { useLocation } from "wouter";

import {
  ApiError,
  type InvitationLookup,
  acceptInvitationLoggedIn,
  lookupInvitation,
} from "@/api/auth";
import { useAuth } from "@/auth/AuthContext";

// AcceptInvitation handles /invitations/accept?token=…:
//   anonymous → bounce to /login?invite=TOKEN&next=/invitations/accept?token=…
//   authed    → lookup → accept → reload identity → switch to the new team → /teams/{id}
//
// The Login view already handles the `?invite=` query for the register
// flow; we add `?next=` so a fresh login lands back here to finish.
export default function AcceptInvitation() {
  const { status, reloadIdentity, selectTeam } = useAuth();
  const [, navigate] = useLocation();
  const [token, setToken] = useState("");
  const [info, setInfo] = useState<InvitationLookup | null>(null);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  // Parse the token once.
  useEffect(() => {
    const u = new URL(window.location.href);
    setToken(u.searchParams.get("token") ?? "");
  }, []);

  // Look up the invitation as soon as we have a token. This works whether
  // or not the user is signed in (the endpoint is public).
  useEffect(() => {
    if (!token) return;
    setErr(null);
    lookupInvitation(token)
      .then(setInfo)
      .catch((e) => {
        const msg = e instanceof ApiError ? e.message : errorMessage(e);
        setErr(msg);
      });
  }, [token]);

  // Anonymous → bounce to /login with the invite + return URL.
  useEffect(() => {
    if (status === "anonymous" && token) {
      const next = encodeURIComponent(
        `/invitations/accept?token=${encodeURIComponent(token)}`,
      );
      navigate(`/login?invite=${encodeURIComponent(token)}&next=${next}`, {
        replace: true,
      });
    }
  }, [status, token, navigate]);

  const accept = async () => {
    setBusy(true);
    setErr(null);
    try {
      const mb = await acceptInvitationLoggedIn(token);
      await reloadIdentity();
      try {
        await selectTeam(mb.team_id);
      } catch {
        // Ignore — reloadIdentity may have already picked the team.
      }
      navigate(`/teams/${mb.team_id}`, { replace: true });
    } catch (e) {
      const msg = e instanceof ApiError ? e.message : errorMessage(e);
      setErr(msg);
    } finally {
      setBusy(false);
    }
  };

  if (status === "loading") {
    return (
      <div
        className="min-h-screen flex items-center justify-center gap-2 bg-surface-0 text-fg-default"
        aria-live="polite"
      >
        <Spinner size="sm" />
        <span>Loading…</span>
      </div>
    );
  }
  if (status === "anonymous") {
    return (
      <div
        className="min-h-screen flex items-center justify-center gap-2 bg-surface-0 text-fg-default"
        aria-live="polite"
      >
        <Spinner size="sm" />
        <span>Redirecting to sign-in…</span>
      </div>
    );
  }

  return (
    <div className="min-h-screen flex items-center justify-center bg-surface-0 text-fg-default px-4">
      <div className="w-full max-w-md bg-surface-1 border border-border-subtle rounded-[var(--radius-lg)] p-8 shadow-[var(--shadow-lg)] space-y-4">
        <h1 className="text-headline font-semibold">Join a team</h1>
        {!token && (
          <InlineBanner tone="danger" layout="inline">
            No invitation token found in the URL.
          </InlineBanner>
        )}
        {err && (
          <InlineBanner tone="danger" layout="inline">
            {err}
          </InlineBanner>
        )}
        {info && !err && (
          <div className="space-y-2 text-sm">
            <div>
              <span className="text-fg-muted">Team:</span>{" "}
              <span className="font-medium">{info.team_name}</span>
            </div>
            <div>
              <span className="text-fg-muted">Role:</span>{" "}
              <span className="font-medium">{info.role}</span>
            </div>
            <div>
              <span className="text-fg-muted">Invited email:</span>{" "}
              <span className="font-mono text-xs">{info.email}</span>
            </div>
            <Button
              variant="primary"
              onClick={() => void accept()}
              loading={busy}
              className="w-full mt-3"
            >
              {busy ? "Joining…" : `Join ${info.team_name}`}
            </Button>
          </div>
        )}
        <div className="text-sm text-fg-muted text-center">
          <Button
            variant="ghost"
            size="sm"
            onClick={() => navigate("/")}
          >
            Cancel
          </Button>
        </div>
      </div>
    </div>
  );
}
