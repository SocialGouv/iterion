import { errorMessage } from "@/lib/errorHints";
import { formatCredentialFingerprint, formatDateTime } from "@/lib/format";
import { useState } from "react";
import { Select } from "@/components/ui/Select";
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

type OAuthConnectionsProps = { scope?: OAuthScope; org?: boolean };

export default function OAuthConnections(props: OAuthConnectionsProps) {
  const scope = props.scope ?? { mine: true };
  const key = "teamId" in scope ? `team:${scope.teamId}` : "platform" in scope ? "platform" : "mine";
  // A pending connect/rename form belongs to one owner. Switching owners
  // must discard it before a subsequent submit can write to the new scope.
  return <ScopedOAuthConnections key={key} {...props} />;
}

function ScopedOAuthConnections({
  scope = { mine: true },
  org = false,
}: OAuthConnectionsProps) {
  const isPlatform = "platform" in scope;
  const scopeKey = "teamId" in scope ? scope.teamId : isPlatform ? "platform" : "mine";
  const queryClient = useQueryClient();
  const query = useQuery<OAuthConnection[]>({
    queryKey: ["oauth-connections", scopeKey],
    queryFn: () => listOAuthConnections(scope),
  });
  const conns = query.data ?? [];
  const [selectedRanks, setSelectedRanks] = useState<Record<string, number>>({});
  const rankFor = (kind: OAuthKind) => selectedRanks[`${scopeKey}:${kind}`] ?? 0;
  const selectRank = (kind: OAuthKind, rank: number) =>
    setSelectedRanks((prev) => ({ ...prev, [`${scopeKey}:${kind}`]: rank }));
  const lookup = (kind: OAuthKind) => conns.find((c) => c.kind === kind && (c.rank ?? 0) === rankFor(kind));
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

  // Keep a custom display name when rotating this selected chain entry.
  // Provider identity is verified independently of this editable label.
  const openWithCurrentName = (kind: OAuthKind) => {
    setLabel(lookup(kind)?.account_label ?? "");
  };

  // --- browser OAuth (claude_code) ---
  const startConnect = async (kind: OAuthKind) => {
    setBusy(true);
    setMutErr(null);
    try {
      const { authorize_url } = await startOAuthAuthorize(kind, scope, rankFor(kind));
      window.open(authorize_url, "_blank", "noopener,noreferrer");
      setConnecting(kind);
      setCode("");
      openWithCurrentName(kind);
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
      await completeOAuthAuthorize(connecting, { code: code.trim() }, scope, label, rankFor(connecting));
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
      await uploadOAuthCredentials(pasteKind, draft, scope, label, rankFor(pasteKind));
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
      await renameOAuth(renaming, renameDraft, scope, rankFor(renaming));
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
      await refreshOAuth(kind, scope, rankFor(kind));
      reload();
    } catch (e) {
      setMutErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const remove = async (kind: OAuthKind) => {
    const targetRank = rankFor(kind);
    const ok = await confirm({
      title: `Disconnect ${kind}?`,
      message: `You'll need to reconnect to use this subscription again.`,
      confirmLabel: "Disconnect",
      confirmVariant: "danger",
    });
    if (!ok) return;
    try {
      await deleteOAuth(kind, scope, targetRank);
      selectRank(kind, 0);
      reload();
    } catch (e) {
      setMutErr(errorMessage(e));
    }
  };

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
            const chain = conns.filter((c) => c.kind === kind).sort((a, b) => (a.rank ?? 0) - (b.rank ?? 0));
            const ranks = [...new Set([0, ...chain.map((c) => c.rank ?? 0)])];
            const nextRank = Math.max(...ranks) + 1;
            const sameAccount = conn?.account_verified
              ? chain.filter((c) => (c.rank ?? 0) !== (conn.rank ?? 0) && c.fingerprint === conn.fingerprint)
              : [];
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

                <label className="text-xs text-fg-muted flex items-center gap-2">
                  Fallback order
                  <Select
                    aria-label={`${display} chain entry`}
                    className="bg-surface-1 border border-border-subtle rounded px-2 py-1 text-fg"
                    value={rankFor(kind)}
                    disabled={busy || connecting === kind || pasteKind === kind || renaming === kind}
                    onChange={(e) => selectRank(kind, Number(e.target.value))}
                  >
                    {ranks.map((rank) => {
                      const entry = chain.find((c) => (c.rank ?? 0) === rank);
                      return <option key={rank} value={rank}>{rank === 0 ? "Primary" : `Fallback ${rank}`}{entry ? ` — ${entry.account_email || entry.account_label || "unnamed"}` : " — not connected"}</option>;
                    })}
                    <option value={nextRank}>Add fallback {nextRank}</option>
                  </Select>
                </label>

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
                        fp <code className="font-mono">{formatCredentialFingerprint(conn.fingerprint, 11)}</code>
                      </span>
                    )}
                  </div>
                )}

                {conn?.account_verified && (
                  <div className="text-xs text-fg-muted">
                    Verified account: <span className="text-fg">{conn.account_email}</span>
                    {conn.account_checked_at && ` · checked ${formatDateTime(conn.account_checked_at)}`}
                  </div>
                )}
                {conn?.account_error && <InlineBanner tone="warning" layout="inline">{conn.account_error}</InlineBanner>}
                {sameAccount.length > 0 && (
                  <InlineBanner tone="warning" layout="inline">
                    This account also occupies {sameAccount.map((c) => c.rank === 0 ? "the primary slot" : `fallback ${c.rank}`).join(", ")}. These entries share one provider quota.
                  </InlineBanner>
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
                          openWithCurrentName(kind);
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
                          openWithCurrentName(kind);
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
