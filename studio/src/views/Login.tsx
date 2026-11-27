import { errorMessage } from "@/lib/errorHints";
import { LegalLinks } from "@/views/Legal/LegalLinks";
import { useEffect, useState } from "react";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Button } from "@/components/ui/Button";
import { Input } from "@/components/ui/Input";
import { Spinner } from "@/components/ui/Spinner";
import { useLocation } from "wouter";
import {
  ApiError,
  listProviders,
  lookupInvitation,
  type AuthSession,
  type InvitationLookup,
  type ProvidersResponse,
} from "@/api/auth";
import { abandon, canPresent, readRecord, SignOutHeldError } from "@/api/signOutMachine";
import { consumeQueryParams } from "@/lib/queryFlash";
import { useAuth } from "@/auth/AuthContext";
import { signInReturnTo } from "@/auth/returnTo";
import { useServerInfoStore } from "@/store/serverInfo";
import { apiBase, STUDIO_BASE } from "@/lib/scope";

const BASE = apiBase().replace(/\/$/, "");

// ssoErrorNotice maps a backend SSO callback error code (?sso_error=) to a
// friendly, actionable banner. `warning` is recoverable-by-the-user;
// `danger` is a hard failure to retry or report.
function ssoErrorNotice(
  code: string,
  provider: string,
): { tone: "warning" | "danger"; text: string } {
  switch (code) {
    case "link_required":
      return {
        tone: "warning",
        text: `An account already exists with this email. Sign in with your password below, then connect ${provider} from the avatar menu → Account settings.`,
      };
    case "restricted":
      return {
        tone: "warning",
        text: `${provider} sign-in is limited to allow-listed teams, and yours didn't match. Ask your administrator for access.`,
      };
    case "disabled":
      return {
        tone: "warning",
        text: "That SSO provider is currently turned off. Contact your administrator or sign in another way.",
      };
    case "unknown_provider":
      return {
        tone: "danger",
        text: "That SSO provider is no longer available. Try signing in with your email and password.",
      };
    // A provider the server could not READ is not one that is gone: telling
    // the user to stop waiting for an SSO that is merely down sends them to
    // a password they may not have (pkg/server oidc.ErrProviderUnavailable).
    case "provider_unavailable":
      return {
        tone: "danger",
        text: `${provider} sign-in is temporarily unavailable — we couldn't read its configuration. Please try again in a moment; if it persists, contact your administrator.`,
      };
    case "password_change_required":
      return {
        tone: "warning",
        text: `Your administrator requires a password change before your next sign-in. An account that signs in through ${provider} has no password to type here: set one with "Forgot your password?" below — the link is emailed to you — or ask your administrator.`,
      };
    case "state_expired":
      return {
        tone: "warning",
        text: "Your sign-in took too long or the link was reused. Please start again.",
      };
    case "agent_binding":
      return {
        tone: "warning",
        text: "We couldn't safely complete that sign-in (it may have started in another browser). Please try again here.",
      };
    case "provider_error":
    case "exchange_failed":
    case "login_failed":
    default:
      return {
        tone: "danger",
        text: `${provider} sign-in didn't complete. Please try again, or use your email and password.`,
      };
  }
}

// submitOwnsLanding is true while a registration that carried an invitation
// settles. The authenticated state it reaches renders /login anew — another
// SignInCard instance, whose forward to returnTo (the accept page the link
// came from) would race this submit's own landing on the joined team.
let submitOwnsLanding = false;

// LinkInvitation is the invitation the visitor arrived with, as the anonymous
// lookup answered it.
type LinkInvitation =
  | { status: "loading" }
  | { status: "ok"; info: InvitationLookup }
  // accepted: the lookup's 409 — the invitation was accepted already, by
  // whom it does not say. Its acceptee signs in and the accept page opens
  // the team it joined.
  | { status: "accepted" }
  | { status: "error"; message: string };

// invitationLanding is where a registration that carried `invitation` lands.
// A registration that honoured it answers a session whose active team is the
// team it joined: the account lands there, the invitation already accepted.
// That is known when the session's team is the one the lookup named for this
// very token (`knownTeam`), or — no lookup describing the token — when the
// instance is invite-only, where a registration always claims its
// invitation. Otherwise (an open sign-up registers without reading the
// invitation, or the mode is unknown) the accept page takes it: it accepts a
// pending invitation once, and opens the team of one this account accepted.
function invitationLanding(
  session: AuthSession,
  invitation: string,
  knownTeam: string | null,
  inviteOnly: boolean,
): string {
  const joined = session.active_team_id;
  if (joined && (knownTeam === joined || (knownTeam === null && inviteOnly))) {
    return `${STUDIO_BASE}/teams/${encodeURIComponent(joined)}`;
  }
  return `/invitations/accept?token=${encodeURIComponent(invitation)}`;
}

// SignInCard is the email/password + SSO sign-in panel, extracted from
// Login so it can be composed both as a standalone full-screen page
// (Login, below) and beneath the cloud marketing hero (CloudLanding).
export function SignInCard() {
  const { signIn, signUp, status, retrySignOut } = useAuth();
  const [, navigate] = useLocation();
  const returnTo = signInReturnTo(window.location);
  const serverInfo = useServerInfoStore((s) => s.info);
  // The invitation link's token (?invite=), read once: a visitor arriving by
  // it registers by default, for the invited address only.
  const [linkToken] = useState(() => new URL(window.location.href).searchParams.get("invite") ?? "");
  const [linkInvitation, setLinkInvitation] = useState<LinkInvitation | null>(
    linkToken ? { status: "loading" } : null,
  );
  // manual: the visitor left the link's locked form for the free one (another
  // token or another address), the form a visitor without a link gets.
  const [manual, setManual] = useState(false);
  const byLink = linkToken !== "" && !manual;
  // The link's form opens only on an invitation the lookup described.
  const linkPending = byLink && (linkInvitation?.status === "loading" || linkInvitation?.status === "error");
  // The address is locked to the one the lookup named, when it named one.
  const emailLocked = byLink && linkInvitation?.status === "ok";
  const [mode, setMode] = useState<"login" | "register">(linkToken ? "register" : "login");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  // signInHeld: a password/registration submit the machine held because an
  // unconfirmed sign-out still stands and this browser still holds the named
  // session (I4). null once the explicit way past it is taken.
  const [signInHeld, setSignInHeld] = useState<{ expiresAt: string | null } | null>(null);
  const [name, setName] = useState("");
  const [invitation, setInvitation] = useState(linkToken);
  const [providers, setProviders] = useState<ProvidersResponse | null>(null);
  const [org, setOrg] = useState("");
  const [showOrgInput, setShowOrgInput] = useState(false);
  // One enum instead of parallel booleans: discovery is idle / fetching /
  // failed, never an impossible combination.
  const [discovery, setDiscovery] = useState<"idle" | "loading" | "error">("idle");
  const [redirecting, setRedirecting] = useState(false);
  // The provider whose start is held by an unconfirmed sign-out this browser
  // still holds the named session of (oidcStart): null when none is.
  const [ssoHeld, setSsoHeld] = useState<{ name: string; display: string; expiresAt: string | null } | null>(null);
  const [ssoNotice, setSsoNotice] = useState<{
    tone: "warning" | "danger";
    text: string;
  } | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  // Discover SSO providers as the user types their email (or an explicit org
  // slug), debounced. The backend folds an org's own SSO (its Keycloak) in
  // alongside the global providers, resolving the org from the email's verified
  // domain — so a user never has to know their org slug. An unknown
  // email/domain returns only the globals (no org-existence oracle). One
  // debounced path covers both the empty form (globals only) and a typed
  // email, so pre-"@" keystrokes don't each fire a request.
  const emailForDiscovery = email.includes("@") ? email.trim() : "";
  const lookingUpOrg = emailForDiscovery !== "" || org.trim() !== "";
  useEffect(() => {
    setDiscovery("loading");
    const t = setTimeout(() => {
      void listProviders({
        email: emailForDiscovery || undefined,
        org: org.trim() || undefined,
      })
        .then((p) => {
          setProviders(p);
          setDiscovery("idle");
        })
        .catch(() => setDiscovery("error"));
    }, 300);
    return () => clearTimeout(t);
  }, [emailForDiscovery, org]);

  // Surface a friendly banner when the OIDC callback bounced back here with a
  // stable ?sso_error= code (see pkg/server redirectSSOError). The raw provider
  // detail stays server-side; this maps the code to an actionable message.
  useEffect(() => {
    const f = consumeQueryParams(["sso_error", "sso_provider"]);
    if (f.sso_error) {
      setSsoNotice(ssoErrorNotice(f.sso_error, f.sso_provider || "your provider"));
    }
  }, []);

  // Ensure serverInfo is loaded so we can gate the forgot-password link.
  useEffect(() => {
    if (!serverInfo) void useServerInfoStore.getState().refresh();
  }, [serverInfo]);

  useEffect(() => {
    if (status === "authenticated" && !submitOwnsLanding) {
      navigate(returnTo, { replace: true });
    }
  }, [status, navigate, returnTo]);

  // Read the link's invitation: the form names it and takes its address. A
  // lookup that fails is shown as such — the link's form does not open on an
  // invitation the server refuses.
  useEffect(() => {
    // A signed-in visitor is forwarded (above): the lookup is the anonymous
    // form's.
    if (!linkToken || status === "authenticated") return;
    let live = true;
    lookupInvitation(linkToken).then(
      (info) => {
        if (!live) return;
        setLinkInvitation({ status: "ok", info });
        setEmail(info.email);
      },
      (e: unknown) => {
        if (!live) return;
        if (e instanceof ApiError && e.status === 409) {
          // Already accepted: the way forward is its acceptee signing in,
          // with whatever address they hold — the lookup names none.
          setLinkInvitation({ status: "accepted" });
          setMode("login");
          return;
        }
        setLinkInvitation({ status: "error", message: e instanceof ApiError ? e.message : errorMessage(e) });
      },
    );
    return () => {
      live = false;
    };
  }, [linkToken, status]);

  const submit = async (ev: React.FormEvent) => {
    ev.preventDefault();
    await doSubmit(false);
  };

  // doSubmit is the credential step under the machine's SIGN-IN gate: the
  // first attempt presents any pending sign-out and is HELD while it stays
  // unconfirmed and this browser still holds the named session; the second
  // runs on the user's explicit "sign in anyway", which abandons the record
  // (its notice stays, I2) and lets this sign-in land.
  const doSubmit = async (anyway: boolean) => {
    setErr(null);
    setSignInHeld(null);
    setBusy(true);
    try {
      if (mode === "login") {
        await signIn(email, password, { anyway });
        navigate(returnTo, { replace: true });
        return;
      }
      const carried = invitation.trim();
      if (!carried) {
        await signUp({ email, password, name }, { anyway });
        navigate(returnTo, { replace: true });
        return;
      }
      // A registration that carried an invitation lands where its session
      // says (invitationLanding) — never back on an accept page whose
      // invitation the registration already consumed. The landing is this
      // submit's: the sign-in card the authenticated state may mount before
      // it navigates must not forward to returnTo meanwhile.
      const knownTeam =
        linkInvitation?.status === "ok" && carried === linkToken ? linkInvitation.info.team_id : null;
      submitOwnsLanding = true;
      try {
        const session = await signUp({ email, password, name, invitation: carried }, { anyway });
        navigate(invitationLanding(session, carried, knownTeam, providers?.signup_mode === "invite_only"), {
          replace: true,
        });
      } finally {
        submitOwnsLanding = false;
      }
    } catch (e) {
      if (e instanceof SignOutHeldError) {
        setSignInHeld({ expiresAt: e.record.expires_at });
        return;
      }
      // 403 "password change required" → forced rotation flow. Carry the
      // email + the rejected password as the temporary credential so the
      // user only has to type the new one.
      if (
        e instanceof ApiError &&
        e.status === 403 &&
        /password change required/i.test(e.message)
      ) {
        const qs = new URLSearchParams({ email, temp: password, next: returnTo }).toString();
        navigate(`/auth/password/change?${qs}`, { replace: true });
        return;
      }
      const msg = e instanceof ApiError ? e.message : errorMessage(e);
      setErr(msg);
    } finally {
      setBusy(false);
    }
  };

  const redirectToProvider = (name: string) => {
    const next = encodeURIComponent(returnTo);
    window.location.href = `${BASE}/auth/oidc/${encodeURIComponent(name)}/start?next=${next}`;
  };

  // The machine's SSO-START transition, the same rule as the password sign-in:
  // a sign-out still pending is presented first (once the provider's session
  // replaces the cookies, nothing here can end the old one), and while the
  // presentation stays unconfirmed and this browser still holds the named
  // session, the start is held behind the explicit choice — a landing here
  // would otherwise deliver a session the sign-out never got to end (I4). A
  // record whose session is no longer held (+GONE) only warns: the start goes.
  const oidcStart = async (name: string, display: string) => {
    setRedirecting(true);
    setSsoHeld(null);
    await retrySignOut();
    const record = readRecord();
    if (record && canPresent(record)) {
      setRedirecting(false);
      setSsoHeld({ name, display, expiresAt: record.expires_at });
      return;
    }
    redirectToProvider(name);
  };

  // The escape from a held start: "Continue with <provider> anyway" abandons
  // the sign-out for the duration of THIS provider trip — the record moves to
  // ABANDONED (its notice stays in every tab, I2; the abandonment authorised
  // leaving for the provider, nothing else) — and the redirect goes. A trip
  // abandoned at the provider lands back on a browser that still holds the
  // session: the landing refuses to adopt it and re-presents the sign-out.
  const continueWithoutTheSignOut = (name: string) => {
    abandon();
    setSsoHeld(null);
    setRedirecting(true);
    redirectToProvider(name);
  };

  const showRegister = providers?.signup_mode === "open" || invitation !== "";

  return (
    <div className="w-full max-w-md bg-surface-1 border border-border-subtle rounded-[var(--radius-lg)] p-8 shadow-[var(--shadow-lg)]">
        <h1 className="text-headline font-semibold mb-2">
          {mode === "login" ? "Sign in to iterion" : "Create your account"}
        </h1>
        <p className="text-sm text-fg-muted mb-6">
          {mode === "login"
            ? "Use your team email + password, or one of the SSO providers below."
            : invitation
              ? "You're joining a team via invitation."
              : "Sign up for a new iterion workspace."}
        </p>

        {byLink && linkInvitation?.status === "loading" && (
          <div className="flex items-center gap-2 text-sm text-fg-muted mb-4" aria-live="polite">
            <Spinner size="sm" />
            <span>Checking your invitation…</span>
          </div>
        )}
        {byLink && linkInvitation?.status === "error" && (
          <InlineBanner tone="danger" layout="inline" role="alert" className="mb-4">
            This invitation link can't be used: {linkInvitation.message}. Ask the person who invited you for a new
            invitation, or continue without it below.
          </InlineBanner>
        )}
        {byLink && linkInvitation?.status === "accepted" && (
          <InlineBanner tone="info" layout="inline" role="status" className="mb-4">
            This invitation has already been accepted. If you accepted it, sign in to open its team.
          </InlineBanner>
        )}
        {byLink && linkInvitation?.status === "ok" && (
          <InvitationSummary info={linkInvitation.info} />
        )}

        {!linkPending && (
        <form onSubmit={submit} className="space-y-3">
          {mode === "register" && (
            <div>
              <label htmlFor="login-name" className="sr-only">
                Name
              </label>
              <Input
                size="md"
                id="login-name"
                placeholder="Name (optional)"
                value={name}
                onChange={(e) => setName(e.target.value)}
                autoComplete="name"
              />
            </div>
          )}
          <div>
            <label htmlFor="email" className="sr-only">
              Email
            </label>
            <Input
              size="md"
              type="email"
              name="email"
              id="email"
              placeholder="Email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              // The link's invitation is for one address: the server refuses
              // any other, so the form does not offer one.
              readOnly={emailLocked}
              aria-readonly={emailLocked || undefined}
              // "username" (not "email") is the token password managers pair with
              // current-password to recognise a login + offer to save it. On
              // register, "email" is the right collect-an-address semantics.
              autoComplete={mode === "login" ? "username" : "email"}
              required
            />
          </div>
          <div>
            <label htmlFor="password" className="sr-only">
              Password
            </label>
            <Input
              size="md"
              type="password"
              name="password"
              id="password"
              placeholder="Password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete={mode === "login" ? "current-password" : "new-password"}
              required
              // Only enforce a minimum when creating a password; a login must
              // accept whatever length the existing password is.
              minLength={mode === "register" ? 8 : undefined}
            />
          </div>
          {mode === "register" && providers?.signup_mode === "invite_only" && !byLink && (
            <div>
              <label htmlFor="login-invitation" className="sr-only">
                Invitation token
              </label>
              <Input
                size="md"
                id="login-invitation"
                className="font-mono"
                placeholder="Invitation token"
                value={invitation}
                onChange={(e) => setInvitation(e.target.value)}
                required
              />
            </div>
          )}
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
            {busy ? "Working…" : mode === "login" ? "Sign in" : "Create account"}
          </Button>
        </form>
        )}
        {byLink && (
          <div className="mt-2 text-center">
            <Button variant="ghost" size="sm" onClick={() => setManual(true)}>
              Use another invitation or address
            </Button>
          </div>
        )}

        <div className="mt-6 space-y-2">
          <div className="text-xs uppercase tracking-wider text-fg-muted">
            Single sign-on
          </div>

          {ssoNotice && (
            <InlineBanner tone={ssoNotice.tone} layout="inline">
              {ssoNotice.text}
            </InlineBanner>
          )}

          {signInHeld && (
            <InlineBanner tone="warning" layout="inline" role="alert">
              Your last sign-out is not confirmed and this browser still holds that session, so this sign-in is on
              hold: it would replace the session you asked to end. Retry the sign-out above, or sign in anyway — the
              session you asked to end stays active
              {signInHeld.expiresAt ? ` until ${signInHeld.expiresAt}` : " until its expiry"}.
            </InlineBanner>
          )}
          {signInHeld && (
            <Button variant="secondary" className="w-full" onClick={() => void doSubmit(true)}>
              Sign in anyway — the session you signed out stays active
              {signInHeld.expiresAt ? ` until ${signInHeld.expiresAt}` : " until its expiry"}
            </Button>
          )}

          {ssoHeld && (
            <InlineBanner
              tone="warning"
              layout="inline"
              role="alert"
              action={
                <Button
                  variant="secondary"
                  size="sm"
                  onClick={() => continueWithoutTheSignOut(ssoHeld.name)}
                >
                  Continue with {ssoHeld.display} anyway
                </Button>
              }
            >
              Your last sign-out is not confirmed and this browser still holds that session, so signing in with{" "}
              {ssoHeld.display} is on hold: it would end the session {ssoHeld.display} opens instead. Retry the
              sign-out above, or continue anyway — the session you asked to end stays active
              {ssoHeld.expiresAt ? ` until ${ssoHeld.expiresAt}` : " until its expiry"}.
            </InlineBanner>
          )}

          {redirecting ? (
            <div className="text-sm text-fg-muted py-1" aria-live="polite">
              Redirecting to your provider…
            </div>
          ) : (providers?.providers?.length ?? 0) > 0 ? (
            <div className="space-y-2">
              {providers!.providers.map((p) => (
                <Button
                  key={p.name}
                  variant="secondary"
                  className="w-full"
                  onClick={() => void oidcStart(p.name, p.display)}
                >
                  Continue with {p.display}
                </Button>
              ))}
            </div>
          ) : discovery === "loading" && lookingUpOrg ? (
            <div className="text-xs text-fg-muted">Looking for your organization's SSO…</div>
          ) : discovery === "error" ? (
            <InlineBanner tone="danger" layout="inline">
              Couldn't reach the server to check for SSO. Check your connection
              and try again.
            </InlineBanner>
          ) : lookingUpOrg ? (
            <div className="text-xs text-fg-muted">
              No SSO is configured for that {org.trim() ? "organization" : "email"}.
              Use your password above, or try a different address.
            </div>
          ) : (
            <div className="text-xs text-fg-muted">
              Enter your work email above and your organization's SSO appears
              here automatically.
            </div>
          )}

          {/* Fallback for users whose org hasn't verified their email domain:
              let them name the org explicitly. Hidden by default to keep the
              common path (email-driven discovery) clean. */}
          {!redirecting &&
            (showOrgInput ? (
              <div className="pt-1">
                <label htmlFor="login-org" className="sr-only">
                  Organization slug
                </label>
                <Input
                  size="md"
                  id="login-org"
                  placeholder="Organization slug (e.g. acme)"
                  value={org}
                  onChange={(e) => setOrg(e.target.value)}
                  autoComplete="organization"
                  autoFocus
                />
              </div>
            ) : (
              <Button
                variant="ghost"
                size="sm"
                onClick={() => setShowOrgInput(true)}
              >
                Sign in with a different organization
              </Button>
            ))}
        </div>

        {!linkPending && (
        <div className="mt-6 text-sm text-fg-muted text-center space-y-1">
          {mode === "login" ? (
            <>
              {showRegister && (
                <div>
                  <Button
                    variant="ghost"
                    size="sm"
                    onClick={() => setMode("register")}
                  >
                    Need an account? Sign up
                  </Button>
                </div>
              )}
              {/* Always reachable: when email is disabled the page
                  explains the admin-assisted path instead of hiding
                  the concept entirely. */}
              <div>
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => navigate("/auth/forgot-password")}
                >
                  Forgot your password?
                </Button>
              </div>
            </>
          ) : (
            <Button
              variant="ghost"
              size="sm"
              onClick={() => setMode("login")}
            >
              Already have an account? Sign in
            </Button>
          )}
        </div>
        )}
        {/* The legal links a visitor is owed BEFORE signing in (ADR-160);
            inside the card so the plain Login page and the product home's
            card both carry them. */}
        <LegalLinks className="mt-6" />
      </div>
  );
}

// InvitationSummary names the invitation a link carries: the team and org it
// joins, the roles it grants and who sent it.
function InvitationSummary({ info }: { info: InvitationLookup }) {
  return (
    <dl
      aria-label="Invitation"
      className="mb-4 grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 rounded-[var(--radius-md)] border border-border-subtle bg-surface-0 p-3 text-sm"
    >
      <dt className="text-fg-muted">Team</dt>
      <dd className="font-medium">{info.team_name}</dd>
      {info.org_name && (
        <>
          <dt className="text-fg-muted">Organization</dt>
          <dd className="font-medium">{info.org_name}</dd>
        </>
      )}
      <dt className="text-fg-muted">Team role</dt>
      <dd className="font-medium">{info.role}</dd>
      {info.org_role && (
        <>
          <dt className="text-fg-muted">Organization role</dt>
          <dd className="font-medium">{info.org_role}</dd>
        </>
      )}
      {info.invited_by && (
        <>
          <dt className="text-fg-muted">Invited by</dt>
          <dd className="font-medium break-all">{info.invited_by}</dd>
        </>
      )}
    </dl>
  );
}

// Login is the standalone full-screen sign-in page (non-cloud, and the
// cloud fallback while server-info is still loading). In cloud mode the
// AuthGate routes anonymous visitors to CloudLanding, which renders the
// marketing hero above this same SignInCard.
export default function Login() {
  return (
    <div className="min-h-screen flex items-center justify-center bg-surface-0 text-fg-default px-4">
      <SignInCard />
    </div>
  );
}
