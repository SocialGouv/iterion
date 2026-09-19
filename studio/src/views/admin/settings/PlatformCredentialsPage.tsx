// Platform-credentials audience console (super-admin, cloud): who may draw on
// the platform LLM tier. Off (enforce false/unset) = every team may; on = only
// the teams listed plus every team of the listed orgs. Save sends only the
// fields the operator changed (merge semantics); teams/orgs replace their list
// wholesale when present.

import { useState } from "react";
import { keepPreviousData, useQuery, useQueryClient } from "@tanstack/react-query";

import {
  FeatureUnavailableError,
  getPlatformCredentialsSettings,
  putPlatformCredentialsSettings,
  type PlatformCredentialsPatch,
} from "@/api/adminSettings";
import { errorMessage } from "@/lib/errorHints";

import { Checkbox } from "@/components/ui/Checkbox";
import { FieldLabel } from "@/components/ui/FieldLabel";
import { TagInput } from "@/components/ui/TagInput";
import { useUIStore } from "@/store/ui";

import { OriginBadge, SettingsScaffold } from "./SettingsScaffold";
import { sameSet } from "./platformCredentials";

export default function PlatformCredentialsPage() {
  const addToast = useUIStore((s) => s.addToast);
  const queryClient = useQueryClient();

  const query = useQuery({
    queryKey: ["admin-platform-credentials"],
    queryFn: getPlatformCredentialsSettings,
    enabled: true,
    placeholderData: keepPreviousData,
  });
  const view = query.data;

  const storedEnforce = view?.stored?.enforce ?? false;
  const storedTeams = view?.stored?.teams ?? [];
  const storedOrgs = view?.stored?.orgs ?? [];
  const recordKey = view ? (view.stored?.updated_at ?? "none") : null;

  const [seededKey, setSeededKey] = useState<string | null>(null);
  const [enforce, setEnforce] = useState(false);
  const [teams, setTeams] = useState<string[]>([]);
  const [orgs, setOrgs] = useState<string[]>([]);
  const [mutErr, setMutErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  if (recordKey != null && recordKey !== seededKey) {
    setSeededKey(recordKey);
    setEnforce(storedEnforce);
    setTeams(storedTeams);
    setOrgs(storedOrgs);
  }

  const buildPatch = (): PlatformCredentialsPatch => {
    const patch: PlatformCredentialsPatch = {};
    if (enforce !== storedEnforce) patch.enforce = enforce;
    if (!sameSet(teams, storedTeams)) patch.teams = teams;
    if (!sameSet(orgs, storedOrgs)) patch.orgs = orgs;
    return patch;
  };
  const patch = buildPatch();
  const dirty = Object.keys(patch).length > 0;

  const save = async () => {
    if (!dirty) return;
    setBusy(true);
    setMutErr(null);
    try {
      await putPlatformCredentialsSettings(patch);
      addToast("Platform credentials audience updated", "success");
      await queryClient.invalidateQueries({ queryKey: ["admin-platform-credentials"] });
    } catch (e) {
      setMutErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <SettingsScaffold
      title="Platform credentials"
      headerRight={<OriginBadge origin={view?.origin} />}
      description="Who may draw on the platform LLM tier. When enforcement is off, every team may. When on, only the teams listed below and every team of the listed orgs may — the rest must bring their own key."
      featureName="Platform-credentials administration"
      unavailableMessage="The /api/admin/settings/platform-credentials endpoint isn't available on this server."
      loaded={!query.isPending}
      unavailable={query.error instanceof FeatureUnavailableError}
      error={query.error}
      fetching={query.isFetching}
      mutError={mutErr}
      saveDisabled={!dirty}
      saving={busy}
      onSave={() => void save()}
      footerLeft={
        <>Currently enforced: {view?.enforced ? "yes" : "no"}</>
      }
    >
      <Checkbox
        label="Enforce the audience (only the teams/orgs below may use the platform tier)"
        checked={enforce}
        disabled={busy}
        onChange={(e) => setEnforce(e.target.checked)}
      />

      <div>
        <FieldLabel htmlFor="pc-teams">Allowed teams</FieldLabel>
        <TagInput value={teams} onChange={setTeams} placeholder="Add a team id…" />
      </div>

      <div>
        <FieldLabel htmlFor="pc-orgs">Allowed orgs</FieldLabel>
        <TagInput value={orgs} onChange={setOrgs} placeholder="Add an org id…" />
        <p className="mt-1 text-caption text-fg-subtle">
          An org admits every one of its teams — the grain an operator usually
          governs at.
        </p>
      </div>
    </SettingsScaffold>
  );
}
