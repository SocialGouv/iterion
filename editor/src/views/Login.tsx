import { useEffect, useState } from "react";
import { useLocation } from "wouter";
import { listProviders, register, type ProvidersResponse } from "@/api/auth";
import { ApiError } from "@/api/auth";
import { useAuth } from "@/auth/AuthContext";

const BASE = (import.meta.env.VITE_API_URL ?? "/api").replace(/\/$/, "");

export default function Login() {
  const { signIn, status } = useAuth();
  const [, navigate] = useLocation();
  const [mode, setMode] = useState<"login" | "register">("login");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [name, setName] = useState("");
  const [invitation, setInvitation] = useState("");
  const [providers, setProviders] = useState<ProvidersResponse | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  useEffect(() => {
    void listProviders().then(setProviders).catch(() => setProviders(null));
  }, []);

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

  const submit = async (ev: React.FormEvent) => {
    ev.preventDefault();
    setErr(null);
    setBusy(true);
    try {
      if (mode === "login") {
        await signIn(email, password);
      } else {
        await register({ email, password, name, invitation: invitation || undefined });
      }
      navigate("/");
    } catch (e) {
      const msg = e instanceof ApiError ? e.message : (e as Error).message;
      setErr(msg);
    } finally {
      setBusy(false);
    }
  };

  const oidcStart = (name: string) => {
    const next = encodeURIComponent("/");
    window.location.href = `${BASE}/auth/oidc/${encodeURIComponent(name)}/start?next=${next}`;
  };

  const showRegister = providers?.signup_mode === "open" || invitation !== "";

  return (
    <div className="min-h-screen flex items-center justify-center bg-surface-0 text-fg-default px-4">
      <div className="w-full max-w-md bg-surface-1 border border-border-subtle rounded-lg p-8 shadow-md">
        <h1 className="text-2xl font-semibold mb-2">
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
            <input
              className="w-full bg-surface-0 border border-border-subtle rounded px-3 py-2"
              placeholder="Name (optional)"
              value={name}
              onChange={(e) => setName(e.target.value)}
              autoComplete="name"
            />
          )}
          <input
            className="w-full bg-surface-0 border border-border-subtle rounded px-3 py-2"
            type="email"
            placeholder="Email"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            autoComplete="email"
            required
          />
          <input
            className="w-full bg-surface-0 border border-border-subtle rounded px-3 py-2"
            type="password"
            placeholder="Password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete={mode === "login" ? "current-password" : "new-password"}
            required
            minLength={8}
          />
          {mode === "register" && providers?.signup_mode === "invite_only" && (
            <input
              className="w-full bg-surface-0 border border-border-subtle rounded px-3 py-2 font-mono text-sm"
              placeholder="Invitation token"
              value={invitation}
              onChange={(e) => setInvitation(e.target.value)}
              required
            />
          )}
          {err && (
            <div className="text-sm text-fg-error bg-surface-warn-subtle border border-border-warn rounded px-3 py-2">
              {err}
            </div>
          )}
          <button
            type="submit"
            disabled={busy}
            className="w-full bg-fg-accent text-surface-0 rounded px-3 py-2 font-medium disabled:opacity-50"
          >
            {busy ? "Working…" : mode === "login" ? "Sign in" : "Create account"}
          </button>
        </form>

        {(providers?.providers?.length ?? 0) > 0 && (
          <div className="mt-6">
            <div className="text-xs uppercase tracking-wider text-fg-muted mb-2">
              Or continue with
            </div>
            <div className="space-y-2">
              {providers!.providers.map((p) => (
                <button
                  key={p.name}
                  onClick={() => oidcStart(p.name)}
                  className="w-full bg-surface-0 border border-border-subtle rounded px-3 py-2 hover:bg-surface-2"
                >
                  {p.display}
                </button>
              ))}
            </div>
          </div>
        )}

        <div className="mt-6 text-sm text-fg-muted text-center">
          {mode === "login" ? (
            showRegister ? (
              <button onClick={() => setMode("register")} className="underline">
                Need an account? Sign up
              </button>
            ) : null
          ) : (
            <button onClick={() => setMode("login")} className="underline">
              Already have an account? Sign in
            </button>
          )}
        </div>
      </div>
    </div>
  );
}
