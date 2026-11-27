import { errorMessage } from "@/lib/errorHints";
import { useCallback, useEffect, useRef, useState } from "react";
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
  teamOfOwnAcceptedInvitation,
} from "@/api/auth";
import { useAuth } from "@/auth/AuthContext";
import { STUDIO_BASE } from "@/lib/scope";

// AcceptInvitation handles /invitations/accept?token=…:
//   anonymous → bounce to /login?invite=TOKEN&next=/invitations/accept?token=…
//   authed    → lookup → accept → reload identity → switch to the new team → /teams/{id}
//   authed, invitation already accepted → ask the accept whether THIS account
//             accepted it: if so, land on its team; otherwise say so.
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
  // ownCheck is the answer to "did this account accept it?" for an
  // invitation the lookup reports already accepted; it is asked once.
  const ownCheckAsked = useRef(false);
  const [ownCheckErr, setOwnCheckErr] = useState<string | null>(null);

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
  // The lookup is anonymous: its 409 says the invitation was accepted, not by
  // whom. A signed-in visitor asks the accept, which names the team to the
  // account that accepted it and to nobody else.
  const alreadyAccepted =
    status === "authenticated" && lookupQuery.error instanceof ApiError && lookupQuery.error.status === 409;
  const lookupErr = lookupQuery.error && !alreadyAccepted
    ? lookupQuery.error instanceof ApiError
      ? lookupQuery.error.message
      : errorMessage(lookupQuery.error)
    : null;
  // An accept failure replaces the lookup result display, like the old
  // shared error slot did.
  const err = acceptErr ?? ownCheckErr ?? lookupErr;

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

  // landOn makes the joined team the active one and opens it: the identity is
  // re-read first so the team list holds the new team.
  const landOn = useCallback(
    async (teamID: string) => {
      await reloadIdentity();
      try {
        await selectTeam(teamID);
      } catch {
        // Ignore — reloadIdentity may have already picked the team.
      }
      navigate(`/teams/${teamID}`, { replace: true });
    },
    [navigate, reloadIdentity, selectTeam],
  );

  useEffect(() => {
    if (!alreadyAccepted || ownCheckAsked.current) return;
    ownCheckAsked.current = true;
    // An accepted invitation grants nothing more: the accept refuses before
    // any write, and its refusal is what names the acceptee's team.
    acceptInvitationLoggedIn(token).then(
      (mb) => landOn(mb.team_id),
      (e: unknown) => {
        const team = teamOfOwnAcceptedInvitation(e);
        if (team) return landOn(team);
        setOwnCheckErr(
          e instanceof ApiError && e.status === 409
            ? "This invitation has already been accepted, and not by this account — or the access it gave was removed since. Ask the person who invited you for a new invitation."
            : e instanceof ApiError
              ? e.message
              : errorMessage(e),
        );
      },
    );
  }, [alreadyAccepted, token, landOn]);

  const accept = async () => {
    setBusy(true);
    setAcceptErr(null);
    try {
      const mb = await acceptInvitationLoggedIn(token);
      await landOn(mb.team_id);
    } catch (e) {
      const team = teamOfOwnAcceptedInvitation(e);
      if (team) {
        await landOn(team);
        return;
      }
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
            {info.org_name && (
              <div>
                <span className="text-fg-muted">Organization:</span>{" "}
                <span className="font-medium">{info.org_name}</span>
              </div>
            )}
            <div>
              <span className="text-fg-muted">Role:</span>{" "}
              <span className="font-medium">{info.role}</span>
            </div>
            {info.org_role && (
              <div>
                <span className="text-fg-muted">Organization role:</span>{" "}
                <span className="font-medium">{info.org_role}</span>
              </div>
            )}
            {info.invited_by && (
              <div>
                <span className="text-fg-muted">Invited by:</span>{" "}
                <span className="font-mono text-xs">{info.invited_by}</span>
              </div>
            )}
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
            // STUDIO_BASE, not "/": this view renders outside the studio
            // router, where "/" is the product home — cancelling an invitation
            // would drop a signed-in operator on the marketing page.
            onClick={() => navigate(STUDIO_BASE)}
          >
            Cancel
          </Button>
        </div>
      </div>
    </div>
  );
}
