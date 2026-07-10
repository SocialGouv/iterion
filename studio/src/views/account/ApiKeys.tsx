import { errorMessage } from "@/lib/errorHints";
import { useEffect, useState } from "react";
import { Button } from "@/components/ui/Button";
import { Checkbox } from "@/components/ui/Checkbox";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { useConfirm } from "@/hooks/useConfirm";
import { EmptyState } from "@/components/ui/EmptyState";
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
  const { confirm, dialog } = useConfirm();
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
      setErr(errorMessage(e));
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
      setErr(errorMessage(e));
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
      setErr(errorMessage(e));
    }
  };

  const remove = async (k: ApiKeyView) => {
    const ok = await confirm({
      title: "Delete API key?",
      message: `Delete API key “${k.name}”? This cannot be undone.`,
      confirmLabel: "Delete",
      confirmVariant: "danger",
    });
    if (!ok) return;
    try {
      await deleteApiKey(team ? { team_id: team.id } : { mine: true }, k.id);
      void reload();
    } catch (e) {
      setErr(errorMessage(e));
    }
  };

  return (
    <div className="space-y-4">
      {dialog}
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-semibold">
          {team ? `${team.name} — Team API keys` : "My API keys"}
        </h2>
        {canManage && (
          <Button
            variant="primary"
            size="sm"
            onClick={() => setShowAdd((v) => !v)}
          >
            {showAdd ? "Cancel" : "Add key"}
          </Button>
        )}
      </div>

      {showAdd && canManage && (
        <form
          onSubmit={submitAdd}
          className="bg-surface-1 border border-border-subtle rounded p-4 space-y-3"
        >
          <div className="grid grid-cols-2 gap-3">
            <Select
              size="md"
              value={draft.provider}
              onChange={(e) => setDraft({ ...draft, provider: e.target.value as Provider })}
            >
              {PROVIDER_OPTIONS.map((p) => (
                <option key={p} value={p}>
                  {p}
                </option>
              ))}
            </Select>
            <Input
              size="md"
              placeholder="Name (e.g. prod-anthropic)"
              value={draft.name}
              onChange={(e) => setDraft({ ...draft, name: e.target.value })}
              required
            />
          </div>
          <Input
            size="md"
            type="password"
            className="font-mono"
            placeholder="API key (sk-ant-… / sk-… / etc.)"
            value={draft.secret}
            onChange={(e) => setDraft({ ...draft, secret: e.target.value })}
            required
            autoComplete="off"
          />
          <label className="flex items-center gap-2 text-sm">
            <Checkbox
              checked={draft.is_default}
              onChange={(e) => setDraft({ ...draft, is_default: e.target.checked })}
            />
            Set as default for this provider
          </label>
          <div className="text-xs text-fg-muted">
            The secret is sealed at rest with the deployment master key and never returned after
            this submission. Display surfaces show only the last four characters and a fingerprint.
          </div>
          <Button
            variant="primary"
            size="sm"
            type="submit"
            loading={adding}
          >
            {adding ? "Saving…" : "Save key"}
          </Button>
        </form>
      )}

      {err && (
        <InlineBanner tone="danger" layout="inline">
          {err}
        </InlineBanner>
      )}

      {loading ? (
        <EmptyState message="Loading…" />
      ) : keys.length === 0 ? (
        <EmptyState message="No keys yet." />
      ) : (
        <div className="overflow-x-auto"><table className="w-full text-sm">
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
                    <Checkbox
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
                    <Button
                      variant="ghost"
                      size="sm"
                      onClick={() => remove(k)}
                      className="text-danger hover:text-danger"
                    >
                      Delete
                    </Button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table></div>
      )}
    </div>
  );
}
