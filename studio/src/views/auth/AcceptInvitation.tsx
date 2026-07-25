import { errorMessage } from "@/lib/errorHints";
import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Button } from "@/components/ui/Button";
import { Input } from "@/components/ui/Input";
import { Spinner } from "@/components/ui/Spinner";
import { useLocation } from "wouter";

import {
  ApiError,
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
  const [pasted, setPasted] = useState("");
  const [acceptErr, setAcceptErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  // Parse the token once.
  useEffect(() => {
    const u = new URL(window.location.href);
    setToken(u.searchParams.get("token") ?? "");
  }, []);

  // Look up the invitation as soon as we have a token. This works whether
  // or not the user is signed in (the endpoint is public).
  const lookupQuery = useQuery({
    queryKey: ["invitation-lookup", token],
    queryFn: () => lookupInvitation(token),
    enabled: token !== "",
  });
  const info = lookupQuery.data ?? null;
  const lookupErr = lookupQuery.error
    ? lookupQuery.error instanceof ApiError
      ? lookupQuery.error.message
      : errorMessage(lookupQuery.error)
    : null;
  // An accept failure replaces the lookup result display, like the old
  // shared error slot did.
  const err = acceptErr ?? lookupErr;

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
    setAcceptErr(null);
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
      setAcceptErr(msg);
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
          <form
            className="space-y-2"
            onSubmit={(ev) => {
              ev.preventDefault();
              const raw = pasted.trim();
              if (!raw) return;
              // Accept either the bare token or a full invite URL —
              // whichever the admin happened to share.
              let extracted = raw;
              const m = raw.match(/[?&]token=([^&\s]+)/);
              if (m?.[1]) extracted = decodeURIComponent(m[1]);
              setToken(extracted);
            }}
          >
            <p className="text-sm text-fg-muted">
              Paste the invitation link or token you received.
            </p>
            <Input
              size="md"
              type="text"
              value={pasted}
              onChange={(e) => setPasted(e.target.value)}
              placeholder="Invite link or token"
              aria-label="Invitation link or token"
            />
            <Button
              variant="primary"
              type="submit"
              disabled={!pasted.trim()}
              className="w-full"
            >
              Continue
            </Button>
          </form>
        )}
        {err && (
          <InlineBanner tone="danger" layout="inline">
            {err}
          </InlineBanner>
        )}
        {token && !info && !err && (
          <div
            className="flex items-center gap-2 text-sm text-fg-muted"
            aria-live="polite"
          >
            <Spinner size="sm" />
            <span>Checking your invitation…</span>
          </div>
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
