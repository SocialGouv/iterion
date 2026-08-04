import { errorMessage } from "@/lib/errorHints";
import { useEffect, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Input } from "@/components/ui/Input";
import { Spinner } from "@/components/ui/Spinner";
import { Tabs } from "@/components/ui/Tabs";
import { useConfirm } from "@/hooks/useConfirm";
import ApiKeysPanel from "./ApiKeys";
import NotificationsPanel from "./NotificationsPanel";
import OAuthConnections from "./OAuthConnections";
import SharedQuota from "./SharedQuota";
import TokensPanel from "./TokensPanel";
import {
  ApiError,
  changeMyPassword,
  listProviders,
  listSSOLinks,
  revokeAllMySessions,
  ssoLinkStartURL,
  unlinkSSO,
  type ProvidersResponse,
  type SSOLink,
} from "@/api/auth";
import { consumeQueryParams } from "@/lib/queryFlash";
import { useAuth } from "@/auth/AuthContext";
import { useServerInfoStore } from "@/store/serverInfo";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";

type Tab = "api-keys" | "oauth" | "shared-quota" | "tokens" | "notifications" | "profile";

export default function SettingsPage() {
  const { user } = useAuth();
  const serverInfo = useServerInfoStore((s) => s.info);
  const [tab, setTab] = useState<Tab>("api-keys");

  // "Account" matches the /account route and the account-chip menu entry
  // ("Account settings") — plain "Settings" collided with the app-settings
  // dialog vocabulary.
  useHeaderSlot({
    left: <span className="text-sm font-semibold">Account</span>,
  });

  // The SSO link/callback bounces back here with ?sso_linked / ?sso_link_error.
  // Land the user on the Profile tab where the connections live.
  useEffect(() => {
    const u = new URL(window.location.href);
    if (u.searchParams.has("sso_linked") || u.searchParams.has("sso_link_error")) {
      setTab("profile");
    }
  }, []);

  const showAuthTabs = serverInfo?.auth_required !== false;

  const tabs: Array<{ id: Tab; label: string }> = [
    { id: "api-keys", label: "API keys (BYOK)" },
    { id: "oauth", label: "OAuth subscriptions" },
  ];
  // Lending a subscription starts by connecting one, so this tab sits
  // next to the connections and only where an auth stack exists.
  if (showAuthTabs) tabs.push({ id: "shared-quota", label: "Share my quota" });
  if (showAuthTabs) tabs.push({ id: "tokens", label: "Access tokens" });
  if (serverInfo?.web_push_enabled) tabs.push({ id: "notifications", label: "Notifications" });
  tabs.push({ id: "profile", label: "Profile" });

  return (
    <div className="h-full overflow-auto">
      <div className="max-w-5xl mx-auto p-3 sm:p-6 grid grid-cols-1 sm:grid-cols-[200px_1fr] gap-4 sm:gap-6">
        <Tabs
          variant="pill"
          value={tab}
          onValueChange={(v) => setTab(v as Tab)}
          items={tabs.map((t) => ({ value: t.id, label: t.label }))}
          listClassName="flex sm:flex-col gap-1 flex-wrap"
          triggerClassName="sm:w-full sm:text-left"
        />

        <div className="bg-surface-0">
          {tab === "api-keys" && <ApiKeysPanel />}
          {tab === "oauth" && <OAuthConnections />}
          {tab === "shared-quota" && showAuthTabs && <SharedQuota />}
          {tab === "tokens" && showAuthTabs && <TokensPanel />}
          {tab === "notifications" && serverInfo?.web_push_enabled && <NotificationsPanel />}
          {tab === "profile" && (
            <div className="space-y-6">
              <section className="space-y-3 text-sm">
                <h2 className="text-lg font-semibold">Profile</h2>
                <div>Email: {user?.email}</div>
                {user?.name && <div>Name: {user.name}</div>}
                {user && user.status !== "active" && (
                  <div>
                    Status:{" "}
                    <Badge
                      size="sm"
                      variant={user.status === "disabled" ? "danger" : "warning"}
                    >
                      {user.status === "disabled"
                        ? "Disabled"
                        : user.status === "pending_password_change"
                          ? "Password change required"
                          : user.status}
                    </Badge>
                  </div>
                )}
                {user?.is_super_admin && (
                  <div className="text-warning-fg">You are a platform super-admin.</div>
                )}
              </section>
              {showAuthTabs && <ConnectedSSOSection userEmail={user?.email} />}
              {showAuthTabs && <ChangePasswordSection />}
              {showAuthTabs && <SignOutEverywhereSection />}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

// ConnectedSSOSection lists the SSO identities linked to the account and lets
// the user connect a new one (the exit from the login "an account already
// exists — link from settings" 409) or disconnect an existing one.
function ConnectedSSOSection({ userEmail }: { userEmail?: string }) {
  const queryClient = useQueryClient();
  const linksQuery = useQuery({
    queryKey: ["sso-links"],
    queryFn: async () => (await listSSOLinks()).links,
  });
  // null = still loading (drives the spinner below), like the old state.
  const links = linksQuery.data ?? null;
  // Connectable providers depend only on the email domain (an org's Keycloak
  // shows up without its slug), so the link-list invalidation never touches
  // them. Discovery failure just leaves the connect buttons out — they're
  // decoration here, not a required surface (pre-existing swallow).
  const connectableQuery = useQuery({
    queryKey: ["sso-providers", userEmail ?? ""],
    queryFn: () =>
      listProviders(userEmail ? { email: userEmail } : undefined)
        .then((p) => p.providers)
        .catch((): ProvidersResponse["providers"] => []),
  });
  const connectable = connectableQuery.data ?? [];
  // Unlink failures land here; the link-list fetch error shares the banner.
  const [mutErr, setMutErr] = useState<string | null>(null);
  const err =
    mutErr ?? (linksQuery.error ? errorMessage(linksQuery.error) : null);
  const [notice, setNotice] = useState<{
    tone: "success" | "danger";
    text: string;
  } | null>(null);
  const { confirm, dialog } = useConfirm();

  useEffect(() => {
    // Surface the post-link callback result, then clear the one-shot params.
    const f = consumeQueryParams(["sso_linked", "sso_link_error"]);
    if (f.sso_linked) {
      setNotice({ tone: "success", text: `Connected ${f.sso_linked}.` });
    } else if (f.sso_link_error) {
      setNotice({
        tone: "danger",
        text:
          f.sso_link_error === "already_linked"
            ? "That SSO identity is already connected to a different account."
            : "Couldn't connect that SSO identity. Please try again.",
      });
    }
  }, []);

  const linkedProviders = new Set((links ?? []).map((l) => l.provider));

  const disconnect = async (l: SSOLink) => {
    const ok = await confirm({
      title: "Disconnect SSO?",
      message: `Remove the ${l.provider} connection (${l.email ?? l.provider_user_id})? You can reconnect it later.`,
      confirmLabel: "Disconnect",
      confirmVariant: "danger",
    });
    if (!ok) return;
    try {
      await unlinkSSO(l.provider, l.provider_user_id);
      setNotice({ tone: "success", text: `Disconnected ${l.provider}.` });
      // providers are domain-derived; only the link list changed
      void queryClient.invalidateQueries({ queryKey: ["sso-links"] });
    } catch (e) {
      setMutErr(errorMessage(e));
    }
  };

  return (
    <section className="space-y-3 text-sm bg-surface-1 border border-border-subtle rounded p-4">
      {dialog}
      <div>
        <h3 className="font-medium">SSO connections</h3>
        <p className="text-xs text-fg-subtle mt-0.5">
          Sign in faster by connecting a single sign-on identity to this account.
        </p>
      </div>

      {notice && (
        <InlineBanner tone={notice.tone} layout="inline">
          {notice.text}
        </InlineBanner>
      )}
      {err && (
        <InlineBanner tone="danger" layout="inline">
          {err}
        </InlineBanner>
      )}

      {links === null ? (
        <div className="text-xs text-fg-muted">
          <Spinner size="sm" label="Loading identities" />
        </div>
      ) : links.length === 0 ? (
        <div className="text-xs text-fg-muted">No SSO identities connected yet.</div>
      ) : (
        <ul className="divide-y divide-border-subtle">
          {links.map((l) => (
            <li
              key={`${l.provider}:${l.provider_user_id}`}
              className="flex items-center justify-between py-2 gap-3"
            >
              <div className="min-w-0">
                <div className="font-medium">{l.provider}</div>
                {l.email && (
                  <div className="text-xs text-fg-muted truncate">{l.email}</div>
                )}
              </div>
              <Button
                variant="secondary"
                size="sm"
                onClick={() => void disconnect(l)}
              >
                Disconnect
              </Button>
            </li>
          ))}
        </ul>
      )}

      {connectable.filter((p) => !linkedProviders.has(p.name)).length > 0 && (
        <div className="space-y-2 pt-1">
          <div className="text-xs text-fg-subtle">Connect another</div>
          <div className="flex flex-wrap gap-2">
            {connectable
              .filter((p) => !linkedProviders.has(p.name))
              .map((p) => (
                <Button
                  key={p.name}
                  variant="secondary"
                  size="sm"
                  onClick={() => {
                    window.location.href = ssoLinkStartURL(p.name);
                  }}
                >
                  Connect {p.display}
                </Button>
              ))}
          </div>
        </div>
      )}
    </section>
  );
}

function ChangePasswordSection() {
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const [ok, setOk] = useState(false);

  const submit = async (ev: React.FormEvent) => {
    ev.preventDefault();
    setErr(null);
    setOk(false);
    if (next.length < 8) {
      setErr("New password must be at least 8 characters.");
      return;
    }
    if (next !== confirm) {
      setErr("The two new-password fields don't match.");
      return;
    }
    setBusy(true);
    try {
      await changeMyPassword(current, next);
      setOk(true);
      setCurrent("");
      setNext("");
      setConfirm("");
    } catch (e) {
      const msg = e instanceof ApiError ? e.message : errorMessage(e);
      setErr(msg);
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="space-y-3 text-sm bg-surface-1 border border-border-subtle rounded p-4">
      <div>
        <h3 className="font-medium">Change password</h3>
        <p className="text-xs text-fg-subtle mt-0.5">
          Other sessions will be revoked. This window continues with a fresh cookie.
        </p>
      </div>
      <form onSubmit={submit} className="space-y-2 max-w-md">
        <div>
          <label htmlFor="settings-current-password" className="sr-only">
            Current password
          </label>
          <Input
            size="md"
            type="password"
            id="settings-current-password"
            placeholder="Current password"
            value={current}
            onChange={(e) => setCurrent(e.target.value)}
            autoComplete="current-password"
            required
          />
        </div>
        <div>
          <label htmlFor="settings-new-password" className="sr-only">
            New password
          </label>
          <Input
            size="md"
            type="password"
            id="settings-new-password"
            placeholder="New password (≥ 8 characters)"
            value={next}
            onChange={(e) => setNext(e.target.value)}
            autoComplete="new-password"
            minLength={8}
            required
          />
        </div>
        <div>
          <label htmlFor="settings-confirm-password" className="sr-only">
            Confirm new password
          </label>
          <Input
            size="md"
            type="password"
            id="settings-confirm-password"
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
        {ok && (
          <div className="text-sm text-success-fg">Password updated.</div>
        )}
        <Button variant="primary" type="submit" loading={busy}>
          {busy ? "Working…" : "Change password"}
        </Button>
      </form>
    </section>
  );
}

function SignOutEverywhereSection() {
  const { signOut } = useAuth();
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const { confirm, dialog } = useConfirm();

  const submit = async () => {
    const ok = await confirm({
      title: "Sign out everywhere?",
      message: "Revoke every signed-in session for this account?",
      confirmLabel: "Sign out everywhere",
      confirmVariant: "danger",
    });
    if (!ok) return;
    setBusy(true);
    setErr(null);
    try {
      await revokeAllMySessions();
      // The server cleared this window's cookies as well; force the
      // local AuthProvider to flip to anonymous so the UI lands on
      // Login immediately rather than waiting for the next 401.
      await signOut();
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="space-y-3 text-sm bg-surface-1 border border-border-subtle rounded p-4">
      {dialog}
      <div>
        <h3 className="font-medium">Sign out everywhere</h3>
        <p className="text-xs text-fg-subtle mt-0.5">
          Use this when a session may be compromised. PATs are not affected — revoke them on the
          tokens tab.
        </p>
      </div>
      {err && (
        <InlineBanner tone="danger" layout="inline">
          {err}
        </InlineBanner>
      )}
      <Button
        variant="danger"
        loading={busy}
        onClick={() => void submit()}
      >
        {busy ? "Working…" : "Sign out everywhere"}
      </Button>
    </section>
  );
}
