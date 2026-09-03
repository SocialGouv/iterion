import { errorMessage } from "@/lib/errorHints";
import { formatDateTime } from "@/lib/format";
import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Badge } from "@/components/ui/Badge";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Button } from "@/components/ui/Button";
import { Textarea } from "@/components/ui/Textarea";
import { Input } from "@/components/ui/Input";
import { useConfirm } from "@/hooks/useConfirm";
import { useUIStore } from "@/store/ui";
import PanelLoading from "@/components/shared/PanelLoading";
import {
  type OAuthConnection,
  type OAuthKind,
  type OAuthScope,
  completeOAuthAuthorize,
  deleteOAuth,
  listOAuthConnections,
  refreshOAuth,
  renameOAuth,
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
// individual licence — an org-shared subscription (forfait) is a dev/test
// convenience, not a production-automation credential.
const ORG_TOS_WARNING =
  "For developing and testing bots only — not intended for fully automated production. A Claude subscription is an individual licence (Anthropic Consumer Terms); use API keys for production automation.";

export default function OAuthConnections({
  scope = { mine: true },
  org = false,
}: {
  scope?: OAuthScope;
  org?: boolean;
}) {
  const isPlatform = "platform" in scope;
  const scopeKey = "teamId" in scope ? scope.teamId : isPlatform ? "platform" : "mine";
  const queryClient = useQueryClient();
  const query = useQuery<OAuthConnection[]>({
    queryKey: ["oauth-connections", scopeKey],
    queryFn: () => listOAuthConnections(scope),
  });
  const conns = query.data ?? [];
  // Every reload (scope switch, post-connect refresh) replaced the panel
  // with the loading state — isFetching keeps that visible.
  const loading = query.isFetching;
  // Mutation failures share the banner with the fetch error (mutation wins,
  // like the old single slot). They're tagged with their scope so a stale
  // one never outlives a scope switch — the manual reload cleared it there.
  // The fetch error hides while a reload is in flight, which the manual
  // reload achieved by clearing it up front.
  const [mutErrTag, setMutErrTag] = useState<{ scope: string; msg: string } | null>(null);
  const setMutErr = (msg: string | null) =>
    setMutErrTag(msg === null ? null : { scope: scopeKey, msg });
  const mutErr = mutErrTag && mutErrTag.scope === scopeKey ? mutErrTag.msg : null;
  const err =
    mutErr ?? (query.error && !loading ? errorMessage(query.error) : null);
  const [busy, setBusy] = useState(false);
  // Browser flow: which kind is mid-connect + the pasted code.
  const [connecting, setConnecting] = useState<OAuthKind | null>(null);
  const [code, setCode] = useState("");
  // Raw-paste fallback editor.
  const [pasteKind, setPasteKind] = useState<OAuthKind | null>(null);
  const [draft, setDraft] = useState("");
  // Account name typed alongside either connect form. Naming at connect
  // time is what keeps a rotation from un-naming: an unnamed re-connect
  // keeps the previous name only when the fingerprint is unchanged.
  const [label, setLabel] = useState("");
  // Inline rename of an already-connected kind.
  const [renaming, setRenaming] = useState<OAuthKind | null>(null);
  const [renameDraft, setRenameDraft] = useState("");
  const { confirm, dialog } = useConfirm();
  const addToast = useUIStore((s) => s.addToast);

  // Post-mutation refresh: clear the shared error slot and refetch the list.
  const reload = () => {
    setMutErr(null);
    void queryClient.invalidateQueries({ queryKey: ["oauth-connections", scopeKey] });
  };

  const onConnected = () => {
    if (org || isPlatform) addToast(ORG_TOS_WARNING, "warning", { persistent: true });
    reload();
  };

  // --- browser OAuth (claude_code) ---
  const startConnect = async (kind: OAuthKind) => {
    setBusy(true);
    setMutErr(null);
    try {
      const { authorize_url } = await startOAuthAuthorize(kind, scope);
      window.open(authorize_url, "_blank", "noopener,noreferrer");
      setConnecting(kind);
      setCode("");
    } catch (e) {
      setMutErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const finishConnect = async (ev: React.FormEvent) => {
    ev.preventDefault();
    if (!connecting) return;
    setBusy(true);
    setMutErr(null);
    try {
      await completeOAuthAuthorize(connecting, { code: code.trim() }, scope, label);
      setConnecting(null);
      setCode("");
      setLabel("");
      onConnected();
    } catch (e) {
      setMutErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  // --- raw paste fallback ---
  const submitPaste = async (ev: React.FormEvent) => {
    ev.preventDefault();
    if (!pasteKind) return;
    setBusy(true);
    setMutErr(null);
    try {
      await uploadOAuthCredentials(pasteKind, draft, scope, label);
      setPasteKind(null);
      setDraft("");
      setLabel("");
      onConnected();
    } catch (e) {
      setMutErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  // Rename: metadata only — the sealed credential is untouched. An empty
  // name clears it, which the form says out loud.
  const submitRename = async (ev: React.FormEvent) => {
    ev.preventDefault();
    if (!renaming) return;
    setBusy(true);
    setMutErr(null);
    try {
      await renameOAuth(renaming, renameDraft, scope);
      setRenaming(null);
      setRenameDraft("");
      reload();
    } catch (e) {
      setMutErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const refresh = async (kind: OAuthKind) => {
    setBusy(true);
    setMutErr(null);
    try {
      await refreshOAuth(kind, scope);
      reload();
    } catch (e) {
      setMutErr(errorMessage(e));
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
      reload();
    } catch (e) {
      setMutErr(errorMessage(e));
    }
  };

  const lookup = (kind: OAuthKind) => conns.find((c) => c.kind === kind);

  return (
    <div className="space-y-4">
      {dialog}
      <div>
        <h2 className="text-lg font-semibold">
          {isPlatform
            ? "Platform subscriptions"
            : org
              ? "Org Claude subscription"
              : "Model subscriptions"}
        </h2>
        <p className="text-sm text-fg-muted mt-1">
          {isPlatform
            ? "The deployment's own fallback forfait: it funds every run that resolved no tenant credential (and that the mutualised pool did not serve). Stored sealed in the database — rotating it here replaces the runner-pod env variable and needs no redeploy."
            : org
              ? "Connect a Claude subscription (forfait) at the org level. It is used as a fallback for automated runs (webhooks, dispatcher, scheduler) whose trigger has no personal subscription — runs launched by a member with their own connection use that instead."
              : "Connect your personal Claude Pro/Max or ChatGPT subscription so iterion can run agents on your behalf via the official Claude Code / Codex CLIs. The blob is sealed at rest."}
        </p>
      </div>

      {(org || isPlatform) && (
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
        <PanelLoading />
      ) : (
        <div className="space-y-4">
          {KINDS.map(({ kind, display, filename, hint, browser }) => {
            const conn = lookup(kind);
            const expiring = conn?.access_token_expires_at
              ? new Date(conn.access_token_expires_at).getTime() - Date.now() < 24 * 3600_000
              : false;
            const notRefreshable = conn ? conn.refreshable === false : false;
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
                  <div className="text-sm flex items-center gap-2">
                    {conn ? (
                      <>
                        <Badge variant={expiring || notRefreshable ? "warning" : "success"}>
                          Connected
                          {conn.access_token_expires_at &&
                            ` · expires ${formatDateTime(conn.access_token_expires_at)}`}
                        </Badge>
                        {notRefreshable && (
                          <Badge variant="warning">Manual reconnect required before expiry</Badge>
                        )}
                      </>
                    ) : (
                      <Badge variant="neutral">Not connected</Badge>
                    )}
                  </div>
                </div>

                {/* Whose subscription this is: the name beside the fingerprint the
                    publisher logs when it picks the credential, so a log line and
                    this card join by eye. */}
                {conn && (
                  <div className="text-xs text-fg-muted flex flex-wrap items-center gap-x-3 gap-y-1">
                    <span>
                      Account:{" "}
                      {conn.account_label ? (
                        <span className="text-fg font-medium">{conn.account_label}</span>
                      ) : (
                        <span className="italic">unnamed — name it so the instance says whose subscription this is</span>
                      )}
                    </span>
                    {conn.fingerprint && (
                      <span title={conn.fingerprint}>
                        fp <code className="font-mono">{conn.fingerprint.slice(0, 11)}</code>
                      </span>
                    )}
                  </div>
                )}

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
                    <Input
                      aria-label="Account name (optional)"
                      placeholder="Account name (optional) — whose subscription is this? e.g. jothedev"
                      value={label}
                      onChange={(e) => setLabel(e.target.value)}
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
                          setLabel("");
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
                    <Input
                      aria-label="Account name (optional)"
                      placeholder="Account name (optional) — whose subscription is this? e.g. jothedev"
                      value={label}
                      onChange={(e) => setLabel(e.target.value)}
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
                          setLabel("");
                        }}
                      >
                        Cancel
                      </Button>
                    </div>
                  </form>
                ) : renaming === kind ? (
                  <form onSubmit={submitRename} className="space-y-2">
                    <label htmlFor={`oauth-label-${kind}`} className="block text-xs text-fg-muted">
                      Name the account behind this credential. Leave it empty to clear the name.
                    </label>
                    <Input
                      id={`oauth-label-${kind}`}
                      placeholder="e.g. jothedev"
                      value={renameDraft}
                      onChange={(e) => setRenameDraft(e.target.value)}
                    />
                    <div className="flex gap-2">
                      <Button variant="primary" type="submit" loading={busy}>
                        {busy ? "Saving…" : renameDraft.trim() ? "Save name" : "Clear name"}
                      </Button>
                      <Button
                        variant="secondary"
                        type="button"
                        onClick={() => {
                          setRenaming(null);
                          setRenameDraft("");
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
                        <Button
                          variant="secondary"
                          onClick={() => {
                            setRenaming(kind);
                            setRenameDraft(conn.account_label ?? "");
                          }}
                          disabled={busy}
                        >
                          {conn.account_label ? "Rename account" : "Name account"}
                        </Button>
                        {!notRefreshable && (
                          <Button variant="secondary" onClick={() => refresh(kind)} disabled={busy}>
                            Refresh tokens
                          </Button>
                        )}
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
