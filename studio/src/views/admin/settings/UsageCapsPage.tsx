// Platform usage-caps console (super-admin, cloud-mode). The DB-backed
// override of the ITERION_USAGE_CAP_5H_PCT / _WEEK_PCT env defaults, effective
// on every replica without a restart. Mirrors the read/gating pattern of the
// sibling admin consoles (UsersAdminPage, DLQAdminPage): gate on server_info
// BEFORE fetching so local mode never fires a doomed request, show a skeleton
// on first load, and a shared error banner.
//
// The caps protect the deployment's OWN subscription window, so a value here is
// a percentage 0–100. Each window can either carry a DB override or inherit the
// env default; "Clear" drops the override (sends null) and the effective value
// falls back to env. Save sends ONLY the windows the operator changed, matching
// the server's merge semantics (an absent field is left untouched).

import { useState } from "react";
import { keepPreviousData, useQuery, useQueryClient } from "@tanstack/react-query";

import { useAuth } from "@/auth/AuthContext";
import {
  FeatureUnavailableError,
  getUsageCaps,
  putUsageCaps,
  type UsageCapsPatch,
} from "@/api/adminSettings";
import { errorMessage } from "@/lib/errorHints";

import { parseWindow, storedOf, type WindowField } from "./usageCaps";

import { Button } from "@/components/ui/Button";
import { EmptyState } from "@/components/ui/EmptyState";
import { FieldLabel } from "@/components/ui/FieldLabel";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Input } from "@/components/ui/Input";
import { TableSkeleton } from "@/components/ui/Table";
import { CloudOnlyNotice } from "@/components/shared/CloudOnlyNotice";
import { useHeaderSlot } from "@/components/shared/useHeaderSlot";
import { useServerInfoStore } from "@/store/serverInfo";
import { useUIStore } from "@/store/ui";

import AdminNav from "../AdminNav";

export default function UsageCapsPage() {
  const { user } = useAuth();
  const isSuper = user?.is_super_admin ?? false;
  const serverInfo = useServerInfoStore((s) => s.info);
  const isCloud = serverInfo?.mode === "cloud";
  const addToast = useUIStore((s) => s.addToast);
  const queryClient = useQueryClient();

  const query = useQuery({
    queryKey: ["admin-usage-caps"],
    queryFn: getUsageCaps,
    enabled: isSuper && isCloud,
    placeholderData: keepPreviousData,
  });
  const view = query.data;
  const loaded = !query.isPending;
  const unavailable = query.error instanceof FeatureUnavailableError;

  // Draft form state, as strings so an empty field ("inherit env") is
  // representable. Seeded from the stored override and re-seeded whenever the
  // fetched record changes identity — done during render (React's "adjust
  // state while rendering" pattern) rather than in an effect, so there is no
  // cascading re-render.
  const asField = (n: number | null) => (n == null ? "" : String(n));
  const recordKey = view ? (view.record?.updated_at ?? "none") : null;
  const [seededKey, setSeededKey] = useState<string | null>(null);
  const [fiveHour, setFiveHour] = useState("");
  const [week, setWeek] = useState("");
  const [mutErr, setMutErr] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  if (recordKey != null && recordKey !== seededKey) {
    setSeededKey(recordKey);
    setFiveHour(asField(storedOf(view, "five_hour_pct")));
    setWeek(asField(storedOf(view, "week_pct")));
  }

  useHeaderSlot({
    left: <span className="text-sm font-semibold">Usage caps</span>,
    right: view ? (
      <span className="text-xs text-fg-muted">source: {view.source}</span>
    ) : null,
  });

  if (!isSuper) {
    return (
      <div className="p-6">
        <p className="text-sm text-fg-muted">Super-admin only.</p>
      </div>
    );
  }

  if (serverInfo && !isCloud) {
    return (
      <div className="h-full overflow-auto">
        <div className="max-w-3xl mx-auto p-3 sm:p-6">
          <CloudOnlyNotice feature="Usage-cap administration" />
        </div>
      </div>
    );
  }

  if (unavailable) {
    return (
      <div className="h-full overflow-auto">
        <div className="max-w-3xl mx-auto p-3 sm:p-6 space-y-4">
          <AdminNav />
          <EmptyState
            title="Usage-cap settings not enabled"
            message="The /api/admin/settings/usage-caps endpoint isn't available on this server."
          />
        </div>
      </div>
    );
  }

  const fetchErr =
    query.error && !unavailable && !query.isFetching ? errorMessage(query.error) : null;
  const err = mutErr ?? fetchErr;

  const fiveParsed = parseWindow(fiveHour);
  const weekParsed = parseWindow(week);
  const fieldError = fiveParsed.error ?? weekParsed.error ?? null;

  // The patch carries only the windows whose value differs from what is stored,
  // so an unchanged window is never part of the request (merge semantics).
  const buildPatch = (): UsageCapsPatch => {
    const patch: UsageCapsPatch = {};
    const storedFive = storedOf(view, "five_hour_pct");
    const storedWeek = storedOf(view, "week_pct");
    if (fiveParsed.value !== storedFive) patch.five_hour_pct = fiveParsed.value;
    if (weekParsed.value !== storedWeek) patch.week_pct = weekParsed.value;
    return patch;
  };
  const patch = buildPatch();
  const dirty = "five_hour_pct" in patch || "week_pct" in patch;

  const save = async () => {
    if (fieldError || !dirty) return;
    setBusy(true);
    setMutErr(null);
    try {
      await putUsageCaps(patch);
      addToast("Usage caps updated", "success");
      await queryClient.invalidateQueries({ queryKey: ["admin-usage-caps"] });
    } catch (e) {
      setMutErr(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="h-full overflow-auto">
      <div className="max-w-3xl mx-auto p-3 sm:p-6 space-y-4">
        <AdminNav />

        <p className="text-caption text-fg-subtle max-w-2xl">
          The share of the deployment&apos;s own subscription window a run may
          consume before the launch guard refuses it. A blank field inherits the
          environment default; a number 0–100 overrides it. Changes take effect
          on every replica within the propagation bound below.
        </p>

        {err && (
          <InlineBanner tone="danger" layout="inline">
            {err}
          </InlineBanner>
        )}

        {!loaded ? (
          <div className="p-3">
            <TableSkeleton rows={2} cols={3} />
          </div>
        ) : (
          <section className="bg-surface-1 border border-border-subtle rounded-[var(--radius-lg)] shadow-[var(--shadow-sm)] p-4 space-y-5">
            <WindowRow
              label="5-hour window"
              field="five_hour_pct"
              value={fiveHour}
              onChange={setFiveHour}
              envDefault={view?.env.five_hour_pct}
              effective={view?.effective.five_hour_pct}
              mode={view?.effective.five_hour_mode}
              disabled={busy}
            />
            <WindowRow
              label="Weekly window"
              field="week_pct"
              value={week}
              onChange={setWeek}
              envDefault={view?.env.week_pct}
              effective={view?.effective.week_pct}
              mode={view?.effective.week_mode}
              disabled={busy}
            />

            {fieldError && (
              <p className="text-xs text-danger">{fieldError}</p>
            )}

            <div className="flex items-center justify-between gap-3 pt-1">
              <span className="text-caption text-fg-subtle">
                Propagation bound: {view?.propagation_bound_seconds ?? 0}s
              </span>
              <Button
                size="sm"
                loading={busy}
                disabled={!dirty || fieldError != null}
                onClick={() => void save()}
              >
                Save
              </Button>
            </div>
          </section>
        )}
      </div>
    </div>
  );
}

function WindowRow({
  label,
  field,
  value,
  onChange,
  envDefault,
  effective,
  mode,
  disabled,
}: {
  label: string;
  field: WindowField;
  value: string;
  onChange: (v: string) => void;
  envDefault: number | undefined;
  effective: number | undefined;
  mode: string | undefined;
  disabled: boolean;
}) {
  const inputId = `usage-cap-${field}`;
  const inheriting = value.trim() === "";
  return (
    <div className="flex flex-wrap items-end gap-4">
      <div className="min-w-[12rem]">
        <FieldLabel htmlFor={inputId}>{label} (%)</FieldLabel>
        <Input
          id={inputId}
          type="number"
          min={0}
          max={100}
          inputMode="numeric"
          placeholder={envDefault != null ? `env: ${envDefault}` : "inherit env"}
          value={value}
          disabled={disabled}
          onChange={(e) => onChange(e.target.value)}
        />
      </div>
      <div className="text-caption text-fg-subtle space-y-0.5 pb-1">
        <div>
          Effective: <span className="text-fg-default">{effective ?? "—"}%</span>
          {mode ? <span className="text-fg-muted"> ({mode})</span> : null}
        </div>
        <div>
          {inheriting ? (
            <span>inheriting env default ({envDefault ?? "—"}%)</span>
          ) : (
            <button
              type="button"
              className="text-accent hover:underline disabled:opacity-60"
              disabled={disabled}
              onClick={() => onChange("")}
            >
              Clear override (inherit env)
            </button>
          )}
        </div>
      </div>
    </div>
  );
}
