import { useEffect, useState } from "react";
import {
  type OAuthConnection,
  type OAuthKind,
  deleteOAuth,
  listOAuthConnections,
  refreshOAuth,
  uploadOAuthCredentials,
} from "@/api/byok";

const KINDS: Array<{
  kind: OAuthKind;
  display: string;
  filename: string;
  hint: string;
}> = [
  {
    kind: "claude_code",
    display: "Claude Code",
    filename: "~/.claude/.credentials.json",
    hint: "Paste the contents of ~/.claude/.credentials.json from a machine where you've signed into Claude Code with your Pro/Max subscription.",
  },
  {
    kind: "codex",
    display: "OpenAI Codex",
    filename: "~/.codex/auth.json",
    hint: "Paste the contents of ~/.codex/auth.json from a machine where you've signed into Codex with your ChatGPT subscription.",
  },
];

export default function OAuthConnections() {
  const [conns, setConns] = useState<OAuthConnection[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);
  const [editingKind, setEditingKind] = useState<OAuthKind | null>(null);
  const [draft, setDraft] = useState("");
  const [busy, setBusy] = useState(false);

  const reload = async () => {
    setLoading(true);
    setErr(null);
    try {
      setConns(await listOAuthConnections());
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void reload();
  }, []);

  const submit = async (ev: React.FormEvent) => {
    ev.preventDefault();
    if (!editingKind) return;
    setBusy(true);
    setErr(null);
    try {
      await uploadOAuthCredentials(editingKind, draft);
      setEditingKind(null);
      setDraft("");
      void reload();
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const refresh = async (kind: OAuthKind) => {
    setBusy(true);
    setErr(null);
    try {
      await refreshOAuth(kind);
      void reload();
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const remove = async (kind: OAuthKind) => {
    if (!confirm(`Disconnect ${kind}? You'll need to re-paste the credentials to reconnect.`)) return;
    try {
      await deleteOAuth(kind);
      void reload();
    } catch (e) {
      setErr((e as Error).message);
    }
  };

  const lookup = (kind: OAuthKind) => conns.find((c) => c.kind === kind);

  return (
    <div className="space-y-4">
      <div>
        <h2 className="text-lg font-semibold">OAuth subscriptions (forfait)</h2>
        <p className="text-sm text-fg-muted mt-1">
          Connect your personal Claude Pro/Max or ChatGPT subscription so iterion can run agents
          on your behalf via the official Claude Code / Codex CLIs. The blob is sealed at rest;
          iterion never reuses your forfait credentials in its in-process LLM client (CGU).
        </p>
      </div>

      {err && (
        <div className="text-sm text-fg-error bg-surface-warn-subtle border border-border-warn rounded px-3 py-2">
          {err}
        </div>
      )}

      {loading ? (
        <div className="text-fg-muted">Loading…</div>
      ) : (
        <div className="space-y-4">
          {KINDS.map(({ kind, display, filename, hint }) => {
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
                      <span className={expiring ? "text-fg-warn" : "text-fg-success"}>
                        Connected
                        {conn.access_token_expires_at &&
                          ` · expires ${new Date(conn.access_token_expires_at).toLocaleString()}`}
                      </span>
                    ) : (
                      <span className="text-fg-muted">Not connected</span>
                    )}
                  </div>
                </div>

                {editingKind === kind ? (
                  <form onSubmit={submit} className="space-y-2">
                    <div className="text-xs text-fg-muted">{hint}</div>
                    <textarea
                      className="w-full bg-surface-0 border border-border-subtle rounded px-3 py-2 font-mono text-xs"
                      rows={6}
                      placeholder='{ "claudeAiOauth": { "accessToken": "...", … } }'
                      value={draft}
                      onChange={(e) => setDraft(e.target.value)}
                      required
                    />
                    <div className="flex gap-2">
                      <button
                        type="submit"
                        disabled={busy}
                        className="bg-fg-accent text-surface-0 rounded px-3 py-1.5 text-sm disabled:opacity-50"
                      >
                        {busy ? "Sealing…" : "Save"}
                      </button>
                      <button
                        type="button"
                        onClick={() => {
                          setEditingKind(null);
                          setDraft("");
                        }}
                        className="bg-surface-0 border border-border-subtle rounded px-3 py-1.5 text-sm"
                      >
                        Cancel
                      </button>
                    </div>
                  </form>
                ) : (
                  <div className="flex gap-2">
                    <button
                      onClick={() => {
                        setEditingKind(kind);
                        setDraft("");
                      }}
                      className="bg-fg-accent text-surface-0 rounded px-3 py-1.5 text-sm"
                    >
                      {conn ? "Update credentials" : "Connect"}
                    </button>
                    {conn && (
                      <>
                        <button
                          onClick={() => refresh(kind)}
                          className="bg-surface-0 border border-border-subtle rounded px-3 py-1.5 text-sm"
                          disabled={busy}
                        >
                          Refresh tokens
                        </button>
                        <button
                          onClick={() => remove(kind)}
                          className="bg-surface-0 border border-border-subtle rounded px-3 py-1.5 text-sm text-fg-error"
                        >
                          Disconnect
                        </button>
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
