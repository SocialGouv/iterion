// Sandbox console (super-admin, cloud): the `sandbox: auto` default image.
// A blank field inherits the env/built-in pin; a ref (ideally an @sha256
// digest) overrides it. Save sends the field only when it changed.

import { useState } from "react";
import { keepPreviousData, useQuery, useQueryClient } from "@tanstack/react-query";

import {
  FeatureUnavailableError,
  getSandboxSettings,
  putSandboxSettings,
} from "@/api/adminSettings";
import { errorMessage } from "@/lib/errorHints";

import { FieldLabel } from "@/components/ui/FieldLabel";
import { Input } from "@/components/ui/Input";
import { useUIStore } from "@/store/ui";

import { OriginBadge, SettingsScaffold } from "./SettingsScaffold";

export default function SandboxPage() {
  const addToast = useUIStore((s) => s.addToast);
  const queryClient = useQueryClient();

  const query = useQuery({
    queryKey: ["admin-sandbox-settings"],
    queryFn: getSandboxSettings,
    enabled: true,
    placeholderData: keepPreviousData,
  });
  const view = query.data;

  const stored = view?.stored?.default_image ?? null;
  const recordKey = view ? (view.stored?.updated_at ?? "none") : null;
  const [seededKey, setSeededKey] = useState<string | null>(null);
  const [image, setImage] = useState("");
  const [mutErr, setMutErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  if (recordKey != null && recordKey !== seededKey) {
    setSeededKey(recordKey);
    setImage(stored ?? "");
  }

  const next = image.trim() === "" ? null : image.trim();
  const dirty = next !== stored;

  const save = async () => {
    if (!dirty) return;
    setBusy(true);
    setMutErr(null);
    try {
      await putSandboxSettings({ default_image: next });
      addToast("Sandbox settings updated", "success");
      await queryClient.invalidateQueries({ queryKey: ["admin-sandbox-settings"] });
    } catch (e) {
      setMutErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const effective = view?.effective_default_image;
  const inheriting = image.trim() === "";

  return (
    <SettingsScaffold
      title="Sandbox"
      headerRight={<OriginBadge origin={view?.origin} />}
      description="The default image for sandboxed runs (sandbox: auto). A blank field inherits the environment default / built-in pin; prefer an @sha256 digest so a redelivered run reruns identically."
      featureName="Sandbox administration"
      unavailableMessage="The /api/admin/settings/sandbox endpoint isn't available on this server."
      loaded={!query.isPending}
      unavailable={query.error instanceof FeatureUnavailableError}
      error={query.error}
      fetching={query.isFetching}
      mutError={mutErr}
      saveDisabled={!dirty}
      saving={busy}
      onSave={() => void save()}
    >
      <div>
        <FieldLabel htmlFor="sandbox-default-image">Default image</FieldLabel>
        <Input
          id="sandbox-default-image"
          size="md"
          placeholder={
            effective ? `effective: ${effective}` : "inherit env / built-in pin"
          }
          value={image}
          disabled={busy}
          onChange={(e) => setImage(e.target.value)}
        />
        <div className="mt-1 text-caption text-fg-subtle space-y-0.5">
          <div>
            Effective:{" "}
            <span className="text-fg-default break-all">
              {effective && effective !== "" ? effective : "(env / built-in)"}
            </span>
          </div>
          <div>
            {inheriting ? (
              <span>inheriting env default</span>
            ) : (
              <button
                type="button"
                className="text-accent hover:underline disabled:opacity-60"
                disabled={busy}
                onClick={() => setImage("")}
              >
                Clear override (inherit env)
              </button>
            )}
          </div>
        </div>
      </div>
    </SettingsScaffold>
  );
}
