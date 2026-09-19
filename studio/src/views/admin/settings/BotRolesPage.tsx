// Bot-roles console (super-admin, cloud): which bot answers each webhook role.
// Each role has a hardcoded default (shown as "effective"); a DB override
// re-points it without a rollout. A blank field inherits the default (the PUT
// sends null); Save sends only the roles the operator changed.

import { useState } from "react";
import { keepPreviousData, useQuery, useQueryClient } from "@tanstack/react-query";

import {
  FeatureUnavailableError,
  getBotRoles,
  putBotRoles,
  type BotRolesPatch,
} from "@/api/adminSettings";
import { errorMessage } from "@/lib/errorHints";

import { FieldLabel } from "@/components/ui/FieldLabel";
import { Input } from "@/components/ui/Input";
import { useUIStore } from "@/store/ui";

import { OriginBadge, SettingsScaffold } from "./SettingsScaffold";
import { BOT_ROLE_FIELDS, storedRole, type BotRoleField } from "./botRoles";

export default function BotRolesPage() {
  const addToast = useUIStore((s) => s.addToast);
  const queryClient = useQueryClient();

  const query = useQuery({
    queryKey: ["admin-bot-roles"],
    queryFn: getBotRoles,
    enabled: true,
    placeholderData: keepPreviousData,
  });
  const view = query.data;

  const recordKey = view ? (view.stored?.updated_at ?? "none") : null;
  const [seededKey, setSeededKey] = useState<string | null>(null);
  const [draft, setDraft] = useState<Record<BotRoleField, string>>({
    reviewer: "",
    revi_converse: "",
    brancher: "",
    implementer: "",
  });
  const [mutErr, setMutErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  if (recordKey != null && recordKey !== seededKey) {
    setSeededKey(recordKey);
    setDraft({
      reviewer: storedRole(view, "reviewer") ?? "",
      revi_converse: storedRole(view, "revi_converse") ?? "",
      brancher: storedRole(view, "brancher") ?? "",
      implementer: storedRole(view, "implementer") ?? "",
    });
  }

  const buildPatch = (): BotRolesPatch => {
    const patch: BotRolesPatch = {};
    for (const { field } of BOT_ROLE_FIELDS) {
      const next = draft[field].trim() === "" ? null : draft[field].trim();
      const stored = storedRole(view, field);
      if (next !== stored) patch[field] = next;
    }
    return patch;
  };
  const patch = buildPatch();
  const dirty = Object.keys(patch).length > 0;

  const save = async () => {
    if (!dirty) return;
    setBusy(true);
    setMutErr(null);
    try {
      await putBotRoles(patch);
      addToast("Bot roles updated", "success");
      await queryClient.invalidateQueries({ queryKey: ["admin-bot-roles"] });
    } catch (e) {
      setMutErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const effective = view?.effective;

  return (
    <SettingsScaffold
      title="Bot roles"
      headerRight={<OriginBadge origin={view?.origin} />}
      description="Which bot answers each webhook role. A blank field inherits the built-in default; a bot id overrides it, effective on every replica without a rollout."
      featureName="Bot-roles administration"
      unavailableMessage="The /api/admin/settings/bot-roles endpoint isn't available on this server."
      loaded={!query.isPending}
      unavailable={query.error instanceof FeatureUnavailableError}
      error={query.error}
      fetching={query.isFetching}
      mutError={mutErr}
      saveDisabled={!dirty}
      saving={busy}
      onSave={() => void save()}
    >
      {BOT_ROLE_FIELDS.map(({ field, label, help }) => {
        const inputId = `bot-role-${field}`;
        const eff = effective ? effective[field] : undefined;
        const inheriting = draft[field].trim() === "";
        return (
          <div key={field} className="flex flex-wrap items-end gap-4">
            <div className="min-w-[16rem]">
              <FieldLabel htmlFor={inputId} help={help}>
                {label}
              </FieldLabel>
              <Input
                id={inputId}
                placeholder={eff ? `default: ${eff}` : "inherit default"}
                value={draft[field]}
                disabled={busy}
                onChange={(e) => setDraft((d) => ({ ...d, [field]: e.target.value }))}
              />
            </div>
            <div className="text-caption text-fg-subtle pb-1 space-y-0.5">
              <div>
                Effective: <span className="text-fg-default">{eff ?? "—"}</span>
              </div>
              <div>
                {inheriting ? (
                  <span>inheriting default</span>
                ) : (
                  <button
                    type="button"
                    className="text-accent hover:underline disabled:opacity-60"
                    disabled={busy}
                    onClick={() => setDraft((d) => ({ ...d, [field]: "" }))}
                  >
                    Clear override (inherit default)
                  </button>
                )}
              </div>
            </div>
          </div>
        );
      })}
    </SettingsScaffold>
  );
}
