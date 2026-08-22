// SessionModelControl is the assistant's model picker.
//
// Until it existed, nobody could say which model the assistant ran on and
// nobody could change it: the bot pins its model and effort behind env vars
// read at SERVER START — not per user, not per session, invisible in the UI.
// Every other launch surface in the product already had per-run overrides.
//
// It renders in two places for one reason each: on the launcher, so the choice
// can be made before the first message; in the session header, so it is
// visible while talking — a change there applies to the NEXT session, because a
// live run keeps the model it started on.

import { useEffect, useState } from "react";

import { backendForModel, modelCapabilityWarning } from "@/api/models";
import ModelPicker from "@/components/models/ModelPicker";
import { Select } from "@/components/ui/Select";
import { useDebounce } from "@/hooks/useDebounce";
import { useModelCatalog } from "@/hooks/useModelCatalog";
import type {
  SessionModelChoice,
  UseSessionModelPrefResult,
} from "@/hooks/useSessionModelPref";

// The `reasoning_effort` ladder (ir.ValidReasoningEfforts). ultracode is a
// mode, not a wire value, and holds only on claude-opus-4-8 — the picker says
// so rather than letting it degrade quietly.
const EFFORTS = ["low", "medium", "high", "xhigh", "max", "ultracode"] as const;

function summarise(choice: SessionModelChoice): string {
  const bits = [choice.model, choice.backend, choice.effort].filter(Boolean);
  return bits.length > 0 ? bits.join(" · ") : "bot default";
}

export interface SessionModelControlProps {
  pref: UseSessionModelPrefResult;
  // liveRun shifts the copy: a change cannot affect the conversation already
  // running, and saying so is the difference between a confusing control and
  // an honest one.
  liveRun?: boolean;
  className?: string;
}

export default function SessionModelControl({
  pref,
  liveRun = false,
  className = "",
}: SessionModelControlProps) {
  const [open, setOpen] = useState(false);
  const [draft, setDraft] = useState<SessionModelChoice>(pref.choice);

  // Re-sync when the stored preference loads or is reset elsewhere.
  useEffect(() => setDraft(pref.choice), [pref.choice]);

  // The extra spec goes into react-query's KEY, so a value that changes on
  // every keystroke is a cache miss on every keystroke — ModelPicker's
  // "Custom…" branch fires onChange per character, so typing one spec meant
  // ~23 authenticated round-trips, each re-running modelcatalog.Build and,
  // past the detect TTL, shelling out to `claude auth status`. Settle first,
  // and only ask about something that could actually be a spec.
  const settledModel = useDebounce(draft.model ?? "", 400);
  const askSpec = /^[^/\s]+\/[^/\s]+/.test(settledModel) ? settledModel : "";
  // The registry is only needed once the operator goes looking.
  const {
    models,
    recommended,
    resolvedDefaultBackend,
    error: catalogError,
  } = useModelCatalog({
    // The stored choice may be a spec outside the curated set; ask for it so
    // it resolves to a real row rather than an orphan option.
    extraSpecs: askSpec ? [askSpec] : undefined,
    enabled: open,
  });

  const selected = models.find((m) => m.spec === draft.model);

  // Pair the chosen spec with a backend that can drive it — derived from the
  // RESOLVED registry entry, in an effect, not at selection time.
  //
  // Doing it in the picker's onChange looked equivalent and was not: a
  // "Custom…" spec is typed by the operator and is therefore absent from
  // `models` at that instant, so the derivation produced "" and the node kept
  // its pinned `backend: "claude_code"` — re-opening the first-node failure
  // this pairing exists to prevent. The spec is fed back as an extraSpec, so
  // the entry lands on the next fetch and the effect picks it up then.
  const derivedBackend = backendForModel(selected, resolvedDefaultBackend);
  useEffect(() => {
    if (!open) return;
    if (!draft.model) {
      if (draft.backend) setDraft((d) => ({ ...d, backend: "" }));
      return;
    }
    if (derivedBackend && derivedBackend !== draft.backend) {
      setDraft((d) => ({ ...d, backend: derivedBackend }));
    }
  }, [open, draft.model, draft.backend, derivedBackend]);

  const warning = modelCapabilityWarning(selected, {
    wantsUltracode: draft.effort === "ultracode",
  });
  // A spec the registry does not know resolves to no entry, so the capability
  // guard above is silent about it AND no backend can be derived for it. Say
  // so rather than letting the next session discover it.
  // Keyed on the SETTLED value, so a half-typed spec does not flash the
  // warning on its way to being valid.
  const unresolved =
    !!draft.model && draft.model === settledModel && !selected && !catalogError;
  const dirty =
    (draft.model ?? "") !== (pref.choice.model ?? "") ||
    (draft.backend ?? "") !== (pref.choice.backend ?? "") ||
    (draft.effort ?? "") !== (pref.choice.effort ?? "");

  return (
    <div className={className}>
      <button
        type="button"
        className="text-micro text-fg-muted hover:text-fg-default cursor-pointer"
        onClick={() => setOpen((v) => !v)}
        aria-expanded={open}
        title="Choose the model this assistant runs on"
      >
        model: <span className="font-mono">{summarise(pref.choice)}</span>
      </button>

      {open && (
        <div className="mt-2 space-y-2 rounded-md border border-border-default bg-surface-1 p-3">
          <p className="text-caption text-fg-subtle">
            {liveRun
              ? "Applies to the next session — the running one keeps the model it started on."
              : "Applies to this and every later session, until you change it."}
          </p>

          {catalogError && (
            <p className="text-caption text-warning">
              Could not load the model registry ({catalogError}) — pick a model
              by typing its <code>provider/model-id</code>.
            </p>
          )}

          <ModelPicker
            value={draft.model ?? ""}
            onChange={(spec) => setDraft((d) => ({ ...d, model: spec }))}
            models={models}
            recommended={recommended}
            inheritLabel="bot default"
            wantsUltracode={draft.effort === "ultracode"}
          />

          {unresolved && (
            <p className="text-caption text-warning">
              <code>{draft.model}</code> is not in the registry, so its
              capabilities and its backend cannot be checked — the session will
              run on the bot&apos;s own backend, which may not be able to drive
              this model.
            </p>
          )}

          <div className="flex items-center gap-2">
            <label className="text-caption text-fg-muted" htmlFor="session-effort">
              Reasoning
            </label>
            <Select
              id="session-effort"
              fit
              value={draft.effort ?? ""}
              onChange={(e) =>
                setDraft((d) => ({ ...d, effort: e.currentTarget.value }))
              }
            >
              <option value="">bot default</option>
              {EFFORTS.map((e) => (
                <option key={e} value={e}>
                  {e}
                </option>
              ))}
            </Select>
            {/* The ultracode caveat has to be visible at the moment of choosing:
                off claude-opus-4-8 it degrades to plain xhigh with no signal
                anywhere (C089), which reads as "the assistant got worse". */}
            {draft.effort === "ultracode" && !warning && (
              <span className="text-caption text-fg-subtle">
                orchestration prerogative — Opus 4.8 only
              </span>
            )}
            {/* The guard above keys on the DRAFT effort, so leaving this on
                "bot default" silenced it — and a bot whose own
                reasoning_effort is ultracode then degrades with no signal at
                all, which is the single most common configuration. The studio
                cannot read the bot's DSL default from here, so say what is
                actually known rather than implying the default is safe. */}
            {!draft.effort && selected && !selected.ultracode_capable && (
              <span className="text-caption text-fg-subtle">
                the bot&apos;s own effort applies — if it is{" "}
                <code>ultracode</code>, it degrades to <code>xhigh</code> here
              </span>
            )}
          </div>

          {warning && (
            <p
              className={
                warning.level === "blocking"
                  ? "text-caption text-danger"
                  : "text-caption text-warning"
              }
              role={warning.level === "blocking" ? "alert" : undefined}
            >
              {warning.level === "blocking" ? "⚠ " : "· "}
              {warning.message}
            </p>
          )}

          {!pref.available && !pref.error && (
            <p className="text-caption text-fg-subtle">
              This server cannot remember the choice — it applies to sessions
              you launch from this tab only.
            </p>
          )}
          {pref.error && (
            <p className="text-caption text-danger">{pref.error}</p>
          )}

          <div className="flex items-center gap-3 pt-1">
            <button
              type="button"
              className="text-micro text-accent-text hover:underline cursor-pointer disabled:opacity-50"
              disabled={!dirty || pref.saving}
              onClick={() => void pref.save(draft)}
            >
              {pref.saving ? "Saving…" : "Save"}
            </button>
            {(pref.set || dirty) && (
              <button
                type="button"
                className="text-micro text-fg-muted hover:text-fg-default cursor-pointer disabled:opacity-50"
                disabled={pref.saving}
                onClick={() => void pref.reset()}
              >
                Back to bot default
              </button>
            )}
            <button
              type="button"
              className="text-micro text-fg-muted hover:text-fg-default cursor-pointer ml-auto"
              onClick={() => {
                setDraft(pref.choice);
                setOpen(false);
              }}
            >
              Close
            </button>
          </div>
        </div>
      )}
    </div>
  );
}
