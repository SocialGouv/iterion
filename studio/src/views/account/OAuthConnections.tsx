import { errorMessage } from "@/lib/errorHints";
import { useEffect, useState } from "react";
import { Badge } from "@/components/ui/Badge";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Button } from "@/components/ui/Button";
import { Textarea } from "@/components/ui/Textarea";
import { Input } from "@/components/ui/Input";
import { useConfirm } from "@/hooks/useConfirm";
import { EmptyState } from "@/components/ui/EmptyState";
import { useUIStore } from "@/store/ui";
import {
  type OAuthConnection,
  type OAuthKind,
  type OAuthScope,
  completeOAuthAuthorize,
  deleteOAuth,
  listOAuthConnections,
  refreshOAuth,
  startOAuthAuthorize,
  uploadOAuthCredentials,
} from "@/api/byok";

const KINDS: Array<{
  kind: OAuthKind;
  display: string;
  filename: string;
  hint: string;
  browser: boolean;
}> = [
  {
    kind: "claude_code",
    display: "Claude Code",
    filename: "Claude Pro / Max subscription",
    hint: "Paste the contents of ~/.claude/.credentials.json from a machine where you've signed into Claude Code.",
    browser: true,
  },
  {
    kind: "codex",
    display: "OpenAI Codex",
    filename: "~/.codex/auth.json",
    hint: "Paste the contents of ~/.codex/auth.json from a machine where you've signed into Codex with your ChatGPT subscription.",
    browser: false,
  },
];

// The ToS caveat shown for the ORG scope only: a Claude subscription is an
// individual licence — an org-shared forfait is a dev/test convenience, not
// a production-automation credential.
const ORG_TOS_WARNING =
  "For developing and testing bots only — not intended for fully automated production. A Claude subscription is an individual licence (Anthropic Consumer Terms); use API keys for production automation.";

export default function OAuthConnections({
  scope = { mine: true },
  org = false,
}: {
  scope?: OAuthScope;
  org?: boolean;
}) {
  const [conns, setConns] = useState<OAuthConnection[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  // Browser flow: which kind is mid-connect + the pasted code.
  const [connecting, setConnecting] = useState<OAuthKind | null>(null);
  const [code, setCode] = useState("");
  // Raw-paste fallback editor.
  const [pasteKind, setPasteKind] = useState<OAuthKind | null>(null);
  const [draft, setDraft] = useState("");
  const { confirm, dialog } = useConfirm();
  const addToast = useUIStore((s) => s.addToast);

  const reload = async () => {
    setLoading(true);
    setErr(null);
    try {
      setConns(await listOAuthConnections(scope));
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void reload();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [org, "teamId" in scope ? scope.teamId : "mine"]);

  const onConnected = () => {
    if (org) addToast(ORG_TOS_WARNING, "warning", { persistent: true });
    void reload();
  };

  // --- browser OAuth (claude_code) ---
  const startConnect = async (kind: OAuthKind) => {
    setBusy(true);
    setErr(null);
    try {
      const { authorize_url } = await startOAuthAuthorize(kind, scope);
      window.open(authorize_url, "_blank", "noopener,noreferrer");
      setConnecting(kind);
      setCode("");
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const finishConnect = async (ev: React.FormEvent) => {
    ev.preventDefault();
    if (!connecting) return;
    setBusy(true);
    setErr(null);
    try {
      await completeOAuthAuthorize(connecting, { code: code.trim() }, scope);
      setConnecting(null);
      setCode("");
      onConnected();
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  // --- raw paste fallback ---
  const submitPaste = async (ev: React.FormEvent) => {
    ev.preventDefault();
    if (!pasteKind) return;
    setBusy(true);
    setErr(null);
    try {
      await uploadOAuthCredentials(pasteKind, draft, scope);
      setPasteKind(null);
      setDraft("");
      onConnected();
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const refresh = async (kind: OAuthKind) => {
    setBusy(true);
    setErr(null);
    try {
      await refreshOAuth(kind, scope);
      void reload();
    } catch (e) {
      setErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const remove = async (kind: OAuthKind) => {
    const ok = await confirm({
      title: `Disconnect ${kind}?`,
      message: `You'll need to reconnect to use this subscription again.`,
      confirmLabel: "Disconnect",
      confirmVariant: "danger",
    });
    if (!ok) return;
    try {
      await deleteOAuth(kind, scope);
      void reload();
    } catch (e) {
      setErr(errorMessage(e));
    }
  };

  const lookup = (kind: OAuthKind) => conns.find((c) => c.kind === kind);

  return (
    <div className="space-y-4">
      {dialog}
      <div>
        <h2 className="text-lg font-semibold">
          {org ? "Org Claude subscription (forfait)" : "OAuth subscriptions (forfait)"}
        </h2>
        <p className="text-sm text-fg-muted mt-1">
          {org
            ? "Connect a Claude subscription at the org level. It is used as a fallback for automated runs (webhooks, dispatcher, scheduler) whose trigger has no personal forfait — runs launched by a member with their own connection use that instead."
            : "Connect your personal Claude Pro/Max or ChatGPT subscription so iterion can run agents on your behalf via the official Claude Code / Codex CLIs. The blob is sealed at rest."}
        </p>
      </div>

      {org && (
        <InlineBanner tone="warning" layout="inline">
          {ORG_TOS_WARNING}
        </InlineBanner>
      )}

      {err && (
        <InlineBanner tone="danger" layout="inline">
          {err}
        </InlineBanner>
      )}

      {loading ? (
        <EmptyState message="Loading…" />
      ) : (
        <div className="space-y-4">
          {KINDS.map(({ kind, display, filename, hint, browser }) => {
            const conn = lookup(kind);
            const expiring = conn?.access_token_expires_at
              ? new Date(conn.access_token_expires_at).getTime() - Date.now() < 24 * 3600_000
              : false;
            return (
              <div
                key={kind}
                className="bg-surface-1 border border-border-subtle rounded p-4 space-y-3"
              >
                <div className="flex items-center justify-between">
                  <div>
                    <h3 className="font-medium">{display}</h3>
                    <div className="text-xs text-fg-muted">{filename}</div>
                  </div>
                  <div className="text-sm">
                    {conn ? (
                      <Badge variant={expiring ? "warning" : "success"}>
                        Connected
                        {conn.access_token_expires_at &&
                          ` · expires ${new Date(conn.access_token_expires_at).toLocaleString()}`}
                      </Badge>
                    ) : (
                      <Badge variant="neutral">Not connected</Badge>
                    )}
                  </div>
                </div>

                {/* Browser flow code-paste panel (claude_code) */}
                {browser && connecting === kind ? (
                  <form onSubmit={finishConnect} className="space-y-2">
                    <p className="text-xs text-fg-muted">
                      A new tab opened on claude.ai. Authorize, then copy the code shown on the
                      callback page and paste it below.
                    </p>
                    <Input
                      aria-label="Authorization code"
                      className="font-mono text-xs"
                      placeholder="paste the code (code#state) here"
                      value={code}
                      onChange={(e) => setCode(e.target.value)}
                      required
                    />
                    <div className="flex gap-2">
                      <Button variant="primary" type="submit" loading={busy}>
                        {busy ? "Connecting…" : "Finish connection"}
                      </Button>
                      <Button
                        variant="secondary"
                        type="button"
                        onClick={() => {
                          setConnecting(null);
                          setCode("");
                        }}
                      >
                        Cancel
                      </Button>
                    </div>
                  </form>
                ) : pasteKind === kind ? (
                  <form onSubmit={submitPaste} className="space-y-2">
                    <label htmlFor={`oauth-creds-${kind}`} className="block text-xs text-fg-muted">
                      {hint}
                    </label>
                    <Textarea
                      id={`oauth-creds-${kind}`}
                      className="font-mono text-xs"
                      rows={6}
                      placeholder='{ "claudeAiOauth": { "accessToken": "...", … } }'
                      value={draft}
                      onChange={(e) => setDraft(e.target.value)}
                      required
                    />
                    <div className="flex gap-2">
                      <Button variant="primary" type="submit" loading={busy}>
                        {busy ? "Sealing…" : "Save"}
                      </Button>
                      <Button
                        variant="secondary"
                        type="button"
                        onClick={() => {
                          setPasteKind(null);
                          setDraft("");
                        }}
                      >
                        Cancel
                      </Button>
                    </div>
                  </form>
                ) : (
                  <div className="flex flex-wrap gap-2">
                    {browser ? (
                      <Button variant="primary" onClick={() => startConnect(kind)} disabled={busy}>
                        {conn ? "Reconnect Claude" : "Connect Claude"}
                      </Button>
                    ) : (
                      <Button
                        variant="primary"
                        onClick={() => {
                          setPasteKind(kind);
                          setDraft("");
                        }}
                      >
                        {conn ? "Update credentials" : "Connect"}
                      </Button>
                    )}
                    {browser && (
                      <Button
                        variant="ghost"
                        onClick={() => {
                          setPasteKind(kind);
                          setDraft("");
                        }}
                      >
                        Advanced: paste file
                      </Button>
                    )}
                    {conn && (
                      <>
                        <Button variant="secondary" onClick={() => refresh(kind)} disabled={busy}>
                          Refresh tokens
                        </Button>
                        <Button variant="danger" onClick={() => remove(kind)}>
                          Disconnect
                        </Button>
                      </>
                    )}
                  </div>
                )}
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}
