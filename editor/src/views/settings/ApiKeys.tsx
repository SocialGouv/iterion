import { useEffect, useState } from "react";
import { useAuth } from "@/auth/AuthContext";
import {
  type ApiKeyView,
  type Provider,
  createMyApiKey,
  createTeamApiKey,
  deleteApiKey,
  listMyApiKeys,
  listTeamApiKeys,
  updateApiKey,
} from "@/api/byok";

const PROVIDER_OPTIONS: Provider[] = [
  "anthropic",
  "openai",
  "bedrock",
  "vertex",
  "azure",
  "openrouter",
  "xai",
];

interface Props {
  // When team is set, manage that team's keys; otherwise manage the
  // current user's personal keys.
  team?: { id: string; name: string };
}

export default function ApiKeysPanel({ team }: Props) {
  const { activeRole, user } = useAuth();
  const [keys, setKeys] = useState<ApiKeyView[]>([]);
  const [loading, setLoading] = useState(true);
  const [err, setErr] = useState<string | null>(null);
  const [showAdd, setShowAdd] = useState(false);
  const [adding, setAdding] = useState(false);
  const [draft, setDraft] = useState({
    provider: "anthropic" as Provider,
    name: "",
    secret: "",
    is_default: false,
  });

  const canManage = team
    ? activeRole === "admin" || activeRole === "owner" || (user?.is_super_admin ?? false)
    : true; // Personal keys are always editable by their owner.

  const reload = async () => {
    setLoading(true);
    setErr(null);
    try {
      const k = team ? await listTeamApiKeys(team.id) : await listMyApiKeys();
      setKeys(k);
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void reload();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [team?.id]);

  const submitAdd = async (ev: React.FormEvent) => {
    ev.preventDefault();
    setAdding(true);
    setErr(null);
    try {
      if (team) {
        await createTeamApiKey(team.id, draft);
      } else {
        await createMyApiKey(draft);
      }
      setShowAdd(false);
      setDraft({ provider: "anthropic", name: "", secret: "", is_default: false });
      void reload();
    } catch (e) {
      setErr((e as Error).message);
    } finally {
      setAdding(false);
    }
  };

  const toggleDefault = async (k: ApiKeyView) => {
    try {
      await updateApiKey(team ? { team_id: team.id } : { mine: true }, k.id, {
        is_default: !k.is_default,
      });
      void reload();
    } catch (e) {
      setErr((e as Error).message);
    }
  };

  const remove = async (k: ApiKeyView) => {
    if (!confirm(`Delete API key “${k.name}”? This cannot be undone.`)) return;
    try {
      await deleteApiKey(team ? { team_id: team.id } : { mine: true }, k.id);
      void reload();
    } catch (e) {
      setErr((e as Error).message);
    }
  };

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-semibold">
          {team ? `${team.name} — Team API keys` : "My API keys"}
        </h2>
        {canManage && (
          <button
            onClick={() => setShowAdd((v) => !v)}
            className="bg-fg-accent text-surface-0 rounded px-3 py-1.5 text-sm"
          >
            {showAdd ? "Cancel" : "Add key"}
          </button>
        )}
      </div>

      {showAdd && canManage && (
        <form
          onSubmit={submitAdd}
          className="bg-surface-1 border border-border-subtle rounded p-4 space-y-3"
        >
          <div className="grid grid-cols-2 gap-3">
            <select
              className="bg-surface-0 border border-border-subtle rounded px-3 py-2"
              value={draft.provider}
              onChange={(e) => setDraft({ ...draft, provider: e.target.value as Provider })}
            >
              {PROVIDER_OPTIONS.map((p) => (
                <option key={p} value={p}>
                  {p}
                </option>
              ))}
            </select>
            <input
              className="bg-surface-0 border border-border-subtle rounded px-3 py-2"
              placeholder="Name (e.g. prod-anthropic)"
              value={draft.name}
              onChange={(e) => setDraft({ ...draft, name: e.target.value })}
              required
            />
          </div>
          <input
            type="password"
            className="w-full bg-surface-0 border border-border-subtle rounded px-3 py-2 font-mono"
            placeholder="API key (sk-ant-… / sk-… / etc.)"
            value={draft.secret}
            onChange={(e) => setDraft({ ...draft, secret: e.target.value })}
            required
            autoComplete="off"
          />
          <label className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              checked={draft.is_default}
              onChange={(e) => setDraft({ ...draft, is_default: e.target.checked })}
            />
            Set as default for this provider
          </label>
          <div className="text-xs text-fg-muted">
            The secret is sealed at rest with the deployment master key and never returned after
            this submission. Display surfaces show only the last four characters and a fingerprint.
          </div>
          <button
            type="submit"
            disabled={adding}
            className="bg-fg-accent text-surface-0 rounded px-3 py-1.5 text-sm disabled:opacity-50"
          >
            {adding ? "Saving…" : "Save key"}
          </button>
        </form>
      )}

      {err && (
        <div className="text-sm text-fg-error bg-surface-warn-subtle border border-border-warn rounded px-3 py-2">
          {err}
        </div>
      )}

      {loading ? (
        <div className="text-fg-muted">Loading…</div>
      ) : keys.length === 0 ? (
        <div className="text-fg-muted text-sm">No keys yet.</div>
      ) : (
        <table className="w-full text-sm">
          <thead className="text-xs uppercase tracking-wider text-fg-muted text-left">
            <tr>
              <th className="px-2 py-1">Provider</th>
              <th className="px-2 py-1">Name</th>
              <th className="px-2 py-1">Last4</th>
              <th className="px-2 py-1">Default</th>
              <th className="px-2 py-1">Created</th>
              <th className="px-2 py-1">Last used</th>
              <th className="px-2 py-1"></th>
            </tr>
          </thead>
          <tbody>
            {keys.map((k) => (
              <tr key={k.id} className="border-t border-border-subtle">
                <td className="px-2 py-2">{k.provider}</td>
                <td className="px-2 py-2">{k.name}</td>
                <td className="px-2 py-2 font-mono">{k.last4 ?? "—"}</td>
                <td className="px-2 py-2">
                  {canManage ? (
                    <input
                      type="checkbox"
                      checked={k.is_default}
                      onChange={() => toggleDefault(k)}
                    />
                  ) : k.is_default ? "✓" : ""}
                </td>
                <td className="px-2 py-2 text-fg-muted">{new Date(k.created_at).toLocaleDateString()}</td>
                <td className="px-2 py-2 text-fg-muted">
                  {k.last_used_at ? new Date(k.last_used_at).toLocaleString() : "—"}
                </td>
                <td className="px-2 py-2 text-right">
                  {canManage && (
                    <button
                      onClick={() => remove(k)}
                      className="text-fg-error hover:underline"
                    >
                      Delete
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
