import { errorMessage } from "@/lib/errorHints";
import { formatDateTime } from "@/lib/format";
import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Button } from "@/components/ui/Button";
import { Checkbox } from "@/components/ui/Checkbox";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { useConfirm } from "@/hooks/useConfirm";
import { EmptyState } from "@/components/ui/EmptyState";
import { Table, THead, Th, TBody, Tr, Td, TableSkeleton } from "@/components/ui/Table";
import { CloudOnlyNotice } from "@/components/shared/CloudOnlyNotice";
import { useAuth } from "@/auth/AuthContext";
import { useServerInfoStore } from "@/store/serverInfo";
import {
  type ApiKeyScope,
  type ApiKeyView,
  type Provider,
  createApiKey,
  deleteApiKey,
  listApiKeys,
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
  "zai",
];

interface Props {
  // When team is set, manage that team's keys; when platform is set
  // (super-admin), manage the deployment's own fallback keys; otherwise
  // manage the current user's personal keys.
  team?: { id: string; name: string };
  platform?: boolean;
}

export default function ApiKeysPanel({ team, platform = false }: Props) {
  const { activeRole, user } = useAuth();
  // BYOK stores (/api/me/api-keys, /api/teams/{id}/api-keys) are only wired
  // in cloud mode — gate on server_info BEFORE fetching so local/desktop
  // mode never fires a doomed 404 request.
  const serverInfo = useServerInfoStore((s) => s.info);
  const isCloud = serverInfo?.mode === "cloud";
  const queryClient = useQueryClient();
  // The three-way scope is derived ONCE; every branch below keys off it.
  const scope: ApiKeyScope = platform
    ? { platform: true }
    : team
      ? { team_id: team.id }
      : { mine: true };
  const teamKey = platform ? "platform" : team?.id ?? "mine";
  const queryKey = ["api-keys", teamKey];
  const title = platform
    ? "Platform API keys"
    : team
      ? `${team.name} — Team API keys`
      : "My API keys";
  const query = useQuery<ApiKeyView[]>({
    queryKey,
    queryFn: () => listApiKeys(scope),
    enabled: isCloud,
  });
  const keys = query.data ?? [];
  // isPending covers the pre-server_info gate (query disabled); isFetching
  // keeps the skeleton on every visible reload (team switch, post-mutation
  // refetch) — matching the manual reload, which always skeletoned.
  const loading = query.isPending || query.isFetching;
  // Mutation failures share the banner with the fetch error (mutation wins,
  // like the old single slot). They're tagged with their scope so a stale
  // one never outlives a team switch — the manual reload cleared it there.
  // The fetch error hides while a reload is in flight, which the manual
  // reload achieved by clearing it up front.
  const [mutErrTag, setMutErrTag] = useState<{ scope: string; msg: string } | null>(null);
  const setMutErr = (msg: string | null) =>
    setMutErrTag(msg === null ? null : { scope: teamKey, msg });
  const mutErr = mutErrTag && mutErrTag.scope === teamKey ? mutErrTag.msg : null;
  const err =
    mutErr ?? (query.error && !loading ? errorMessage(query.error) : null);
  const [showAdd, setShowAdd] = useState(false);
  const [adding, setAdding] = useState(false);
  const { confirm, dialog } = useConfirm();
  const [draft, setDraft] = useState({
    provider: "anthropic" as Provider,
    name: "",
    secret: "",
    is_default: false,
  });

  const canManage = platform
    ? (user?.is_super_admin ?? false) // Platform keys fund every tenant.
    : team
      ? activeRole === "admin" || activeRole === "owner" || (user?.is_super_admin ?? false)
      : true; // Personal keys are always editable by their owner.

  // Post-mutation refresh: clear the shared error slot and refetch the list.
  const reload = () => {
    setMutErr(null);
    void queryClient.invalidateQueries({ queryKey });
  };

  const submitAdd = async (ev: React.FormEvent) => {
    ev.preventDefault();
    setAdding(true);
    setMutErr(null);
    try {
      await createApiKey(scope, draft);
      setShowAdd(false);
      setDraft({ provider: "anthropic", name: "", secret: "", is_default: false });
      reload();
    } catch (e) {
      setMutErr(errorMessage(e));
    } finally {
      setAdding(false);
    }
  };

  const toggleDefault = async (k: ApiKeyView) => {
    try {
      await updateApiKey(scope, k.id, {
        is_default: !k.is_default,
      });
      reload();
    } catch (e) {
      setMutErr(errorMessage(e));
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
      await deleteApiKey(scope, k.id);
      reload();
    } catch (e) {
      setMutErr(errorMessage(e));
    }
  };

  // Deliberate local-mode gate (not an error): no fetch fired, no mutating
  // controls rendered. While server_info is still loading we fall through to
  // the skeleton below instead of flashing this notice.
  if (serverInfo && !isCloud) {
    return (
      <CloudOnlyNotice
        title="API keys (BYOK)"
        feature="Bring-your-own-key management"
        hint={
          serverInfo.secrets_enabled
            ? "In local mode, credentials live in the sealed secret store — see the Secrets view."
            : undefined
        }
      />
    );
  }

  return (
    <div className="space-y-4">
      {dialog}
      <div className="flex items-center justify-between">
        <h2 className="text-lg font-semibold">{title}</h2>
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
        <TableSkeleton rows={3} cols={7} />
      ) : keys.length === 0 ? (
        <EmptyState message="No keys yet." />
      ) : (
        <Table caption={title}>
          <THead>
            <Th>Provider</Th>
            <Th>Name</Th>
            <Th>Last4</Th>
            <Th>Default</Th>
            <Th>Created</Th>
            <Th>Last used</Th>
            <Th align="right" srLabel="Actions" />
          </THead>
          <TBody>
            {keys.map((k) => (
              <Tr key={k.id}>
                <Td>{k.provider}</Td>
                <Td>{k.name}</Td>
                <Td className="font-mono">{k.last4 ?? "—"}</Td>
                <Td>
                  {canManage ? (
                    <Checkbox
                      aria-label={`Default key for ${k.provider}: ${k.name}`}
                      checked={k.is_default}
                      onChange={() => toggleDefault(k)}
                    />
                  ) : k.is_default ? "✓" : ""}
                </Td>
                <Td className="text-fg-muted">{formatDateTime(k.created_at)}</Td>
                <Td className="text-fg-muted">
                  {formatDateTime(k.last_used_at)}
                </Td>
                <Td align="right">
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
                </Td>
              </Tr>
            ))}
          </TBody>
        </Table>
      )}
    </div>
  );
}
