import { errorMessage } from "@/lib/errorHints";
import { useEffect, useState } from "react";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Button } from "@/components/ui/Button";
import { Input } from "@/components/ui/Input";
import { useLocation } from "wouter";
import { listProviders, type ProvidersResponse } from "@/api/auth";
import { ApiError } from "@/api/auth";
import { consumeQueryParams } from "@/lib/queryFlash";
import { useAuth } from "@/auth/AuthContext";
import { useServerInfoStore } from "@/store/serverInfo";
import { apiBase } from "@/lib/scope";

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

// SignInCard is the email/password + SSO sign-in panel, extracted from
// Login so it can be composed both as a standalone full-screen page
// (Login, below) and beneath the cloud marketing hero (CloudLanding).
export function SignInCard() {
  const { signIn, signUp, status } = useAuth();
  const [, navigate] = useLocation();
  const serverInfo = useServerInfoStore((s) => s.info);
  const [mode, setMode] = useState<"login" | "register">("login");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [name, setName] = useState("");
  const [invitation, setInvitation] = useState("");
  const [providers, setProviders] = useState<ProvidersResponse | null>(null);
  const [org, setOrg] = useState("");
  const [showOrgInput, setShowOrgInput] = useState(false);
  // One enum instead of parallel booleans: discovery is idle / fetching /
  // failed, never an impossible combination.
  const [discovery, setDiscovery] = useState<"idle" | "loading" | "error">("idle");
  const [redirecting, setRedirecting] = useState(false);
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
    if (status === "authenticated") {
      navigate("/");
    }
  }, [status, navigate]);

  // Pre-fill invitation token from URL.
  useEffect(() => {
    const url = new URL(window.location.href);
    const t = url.searchParams.get("invite");
    if (t) {
      setMode("register");
      setInvitation(t);
    }
  }, []);

  // returnTo captures the in-app URL the user was on before being
  // bounced to /login (typically by AuthGate on session expiry), OR
  // honours an explicit ?next= param (e.g. /invitations/accept bounces
  // here with `?invite=…&next=/invitations/accept?token=…`). We
  // restrict to relative same-origin paths so a hostile `?next=`
  // injection can't open-redirect after login.
  const returnTo = (): string => {
    const u = new URL(window.location.href);
    const next = u.searchParams.get("next");
    // Reject `//evil` AND `/\evil` (and encoded `/%2f` / `/%5c`): a leading
    // slash followed by a slash OR backslash is normalized by browsers to a
    // protocol-relative URL → off-site open redirect. Only `/` + a normal path
    // char is a safe same-origin target.
    const isSafeLocalPath = (p: string) =>
      p.startsWith("/") && !/^\/[/\\]/.test(p) && !/^\/(%2f|%5c)/i.test(p);
    if (next && isSafeLocalPath(next)) {
      return next;
    }
    const here = window.location.pathname + window.location.search + window.location.hash;
    if (!here || here.startsWith("/login") || here.startsWith("/auth/")) return "/";
    if (!isSafeLocalPath(here)) return "/";
    return here;
  };

  const submit = async (ev: React.FormEvent) => {
    ev.preventDefault();
    setErr(null);
    setBusy(true);
    try {
      if (mode === "login") {
        await signIn(email, password);
      } else {
        await signUp({ email, password, name, invitation: invitation || undefined });
      }
      navigate(returnTo());
    } catch (e) {
      // 403 "password change required" → forced rotation flow. Carry the
      // email + the rejected password as the temporary credential so the
      // user only has to type the new one.
      if (
        e instanceof ApiError &&
        e.status === 403 &&
        /password change required/i.test(e.message)
      ) {
        const qs = new URLSearchParams({ email, temp: password }).toString();
        navigate(`/auth/password/change?${qs}`, { replace: true });
        return;
      }
      const msg = e instanceof ApiError ? e.message : errorMessage(e);
      setErr(msg);
    } finally {
      setBusy(false);
    }
  };

  const oidcStart = (name: string) => {
    setRedirecting(true);
    const next = encodeURIComponent(returnTo());
    window.location.href = `${BASE}/auth/oidc/${encodeURIComponent(name)}/start?next=${next}`;
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
          {mode === "register" && providers?.signup_mode === "invite_only" && (
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

        <div className="mt-6 space-y-2">
          <div className="text-xs uppercase tracking-wider text-fg-muted">
            Single sign-on
          </div>

          {ssoNotice && (
            <InlineBanner tone={ssoNotice.tone} layout="inline">
              {ssoNotice.text}
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
                  onClick={() => oidcStart(p.name)}
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
      </div>
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
