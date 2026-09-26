// Bot-vars console (super-admin, cloud): ITERION_* var overrides applied to
// every run, beating the pod's env. A key present here overrides env; removing
// a key (or blanking it) clears the override (sends null). Save sends only the
// keys that changed.

import { useState } from "react";
import { keepPreviousData, useQuery, useQueryClient } from "@tanstack/react-query";
import { TrashIcon } from "@radix-ui/react-icons";

import {
  FeatureUnavailableError,
  getBotVars,
  putBotVars,
} from "@/api/adminSettings";
import { errorMessage } from "@/lib/errorHints";

import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";
import { Input } from "@/components/ui/Input";
import { useUIStore } from "@/store/ui";

import { OriginBadge, SettingsScaffold } from "./SettingsScaffold";
import { buildBotVarsPatch, newRow, rowsFromVars, type VarRow } from "./botVars";

export default function BotVarsPage() {
  const addToast = useUIStore((s) => s.addToast);
  const queryClient = useQueryClient();

  const query = useQuery({
    queryKey: ["admin-bot-vars"],
    queryFn: getBotVars,
    enabled: true,
    placeholderData: keepPreviousData,
  });
  const view = query.data;
  const stored = view?.stored?.vars;

  const recordKey = view ? (view.stored?.updated_at ?? "none") : null;
  const [seededKey, setSeededKey] = useState<string | null>(null);
  const [rows, setRows] = useState<VarRow[]>([]);
  const [mutErr, setMutErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  if (recordKey != null && recordKey !== seededKey) {
    setSeededKey(recordKey);
    setRows(rowsFromVars(stored));
  }

  const { patch, error: dupError } = buildBotVarsPatch(rows, stored);
  const dirty = Object.keys(patch).length > 0;

  const setRow = (id: string, next: Partial<VarRow>) =>
    setRows((rs) => rs.map((r) => (r.id === id ? { ...r, ...next } : r)));
  const removeRow = (id: string) => setRows((rs) => rs.filter((r) => r.id !== id));
  const addRow = () => setRows((rs) => [...rs, newRow()]);

  const save = async () => {
    if (!dirty || dupError) return;
    setBusy(true);
    setMutErr(null);
    try {
      await putBotVars(patch);
      addToast("Bot vars updated", "success");
      await queryClient.invalidateQueries({ queryKey: ["admin-bot-vars"] });
    } catch (e) {
      setMutErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <SettingsScaffold
      title="Bot vars"
      headerRight={<OriginBadge origin={view?.origin} />}
      description="ITERION_* variable overrides applied to every run, beating the pod's env (a run still falls back to the .bot's :- default for a key set nowhere). Removing a row clears the override."
      featureName="Bot-vars administration"
      unavailableMessage="The /api/admin/settings/bot-vars endpoint isn't available on this server."
      loaded={!query.isPending}
      unavailable={query.error instanceof FeatureUnavailableError}
      error={query.error}
      fetching={query.isFetching}
      mutError={mutErr ?? dupError ?? null}
      saveDisabled={!dirty || dupError != null}
      saving={busy}
      onSave={() => void save()}
      footerLeft={
        <>Propagation bound: {view?.propagation_bound_seconds ?? 0}s</>
      }
    >
      {rows.length === 0 ? (
        <EmptyState message="No var overrides. Add one below." />
      ) : (
        <div className="space-y-2">
          {rows.map((r) => (
            <div key={r.id} className="flex items-center gap-2">
              <Input
                aria-label="Variable name"
                className="font-mono"
                placeholder="ITERION_EXAMPLE"
                value={r.key}
                disabled={busy}
                onChange={(e) => setRow(r.id, { key: e.target.value })}
              />
              <Input
                aria-label="Variable value"
                placeholder="value"
                value={r.value}
                disabled={busy}
                onChange={(e) => setRow(r.id, { value: e.target.value })}
              />
              <Button
                size="sm"
                variant="ghost"
                className="text-danger shrink-0"
                aria-label="Remove variable"
                disabled={busy}
                leadingIcon={<TrashIcon />}
                onClick={() => removeRow(r.id)}
              >
                Remove
              </Button>
            </div>
          ))}
        </div>
      )}

      <div>
        <Button size="sm" variant="secondary" disabled={busy} onClick={addRow}>
          Add variable
        </Button>
      </div>
    </SettingsScaffold>
  );
}
