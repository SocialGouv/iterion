import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";

import { Badge, Button, EmptyState, Input } from "@/components/ui";

import { desktop, type SecretStatus } from "@/lib/desktopBridge";

// ApiKeysEditor never displays secret values — only stored / not-stored /
// shadowed-by-env status, plus an input for entering or replacing the
// value. Used by both Settings → API keys and the Welcome wizard.
export default function ApiKeysEditor() {
  const queryClient = useQueryClient();
  const queryKey = ["secret-statuses"];
  const { data: statuses, error: fetchErrorObj } = useQuery<SecretStatus[]>({
    queryKey,
    queryFn: () => desktop.getSecretStatuses(),
  });
  const fetchError = fetchErrorObj
    ? (fetchErrorObj as Error).message
    : null;
  const refresh = () => queryClient.invalidateQueries({ queryKey });
  const [drafts, setDrafts] = useState<Record<string, string>>({});
  const [mutationError, setMutationError] = useState<string | null>(null);

  if (!statuses) {
    return fetchError ? (
      <EmptyState message={<span className="text-danger">{fetchError}</span>} />
    ) : (
      <EmptyState message="Loading…" />
    );
  }

  const handleSave = async (key: string) => {
    setMutationError(null);
    const v = drafts[key];
    if (!v) return;
    try {
      await desktop.setSecret(key, v);
      setDrafts((d) => ({ ...d, [key]: "" }));
      await refresh();
    } catch (err) {
      setMutationError(err instanceof Error ? err.message : String(err));
    }
  };

  const handleDelete = async (key: string) => {
    setMutationError(null);
    try {
      await desktop.deleteSecret(key);
      await refresh();
    } catch (err) {
      setMutationError(err instanceof Error ? err.message : String(err));
    }
  };

  const error = mutationError ?? fetchError;

  return (
    <div className="flex flex-col gap-2">
      {statuses.map((s) => (
        <div
          key={s.key}
          className="flex items-center gap-3 border-b border-border-default py-2"
        >
          <div className="w-48 shrink-0">
            <div className="text-sm font-semibold">{s.key}</div>
            <SecretBadge status={s} />
          </div>
          <Input
            type="password"
            placeholder={s.stored ? "•••••••• (stored)" : "Enter value"}
            value={drafts[s.key] ?? ""}
            onChange={(e) =>
              setDrafts((d) => ({ ...d, [s.key]: e.target.value }))
            }
          />
          <Button
            size="sm"
            variant="primary"
            disabled={!drafts[s.key]}
            onClick={() => handleSave(s.key)}
          >
            Save
          </Button>
          {s.stored && (
            <Button size="sm" variant="ghost" onClick={() => handleDelete(s.key)}>
              Delete
            </Button>
          )}
        </div>
      ))}
      {error && (
        <p className="text-danger text-sm" role="alert">
          {error}
        </p>
      )}
    </div>
  );
}

function SecretBadge({ status }: { status: SecretStatus }) {
  if (status.shadowed) return <Badge variant="warning">shadowed by env</Badge>;
  if (status.stored) return <Badge variant="success">stored</Badge>;
  return <Badge variant="neutral">not stored</Badge>;
}
