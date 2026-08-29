// ModelPicker is the one model-selection control in the studio: the launch
// form's per-node rows and the assistant session both mount it.
//
// It replaces a free-text field with a datalist of hints. That field could not
// answer the three questions an operator actually has — what do I have access
// to, what does it cost, and can it even do the job — because the hints came
// from the detected providers' `suggested_model` alone. This reads the model
// registry (GET /api/models) instead, so each option carries its reachability,
// context window and price, and the capability guard fires BEFORE launch
// rather than mid-run.
//
// The choice stays free: a spec outside the registry is still reachable through
// the "Custom…" escape hatch, because the DSL accepts any provider/model-id and
// the curated set is explicitly not exhaustive.

import { useState } from "react";

import {
  formatContextWindow,
  formatModelPrice,
  modelCapabilityWarning,
  type ModelEntry,
} from "@/api/models";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";

const CUSTOM = "__custom__";

export interface ModelPickerProps {
  /** The chosen spec, or "" for the inherit/default state. */
  value: string;
  onChange: (spec: string) => void;
  models: ModelEntry[];
  /** The host's own suggestion, kept one click away. */
  recommended?: ModelEntry | null;
  /** Label for the "" option — what leaving this alone actually means. */
  inheritLabel?: string;
  /** True when this node/session runs at reasoning_effort: ultracode. */
  wantsUltracode?: boolean;
  /** Hide the per-model detail line (dense per-node rows). */
  compact?: boolean;
  disabled?: boolean;
  id?: string;
}

// optionLabel packs the decision into one line: the spec, whether it is
// reachable, its context window and its price.
function optionLabel(m: ModelEntry): string {
  const bits: string[] = [];
  if (m.context_window > 0) bits.push(formatContextWindow(m.context_window));
  if (m.price_known) bits.push(formatModelPrice(m).replace(" per Mtok", "/Mtok"));
  if (!m.tool_call) bits.push("no tools");
  if (m.reachability === "unknown") bits.push("unproven");
  else if (!m.usable) bits.push("no credential");
  return bits.length > 0 ? `${m.spec} — ${bits.join(" · ")}` : m.spec;
}

export default function ModelPicker({
  value,
  onChange,
  models,
  recommended,
  inheritLabel = "inherit (bot default)",
  wantsUltracode = false,
  compact = false,
  disabled = false,
  id,
}: ModelPickerProps) {
  const [custom, setCustom] = useState(false);

  const selected = models.find((m) => m.spec === value);
  const warning = modelCapabilityWarning(selected, { wantsUltracode });
  const usable = models.filter((m) => m.usable);
  const unproven = models.filter((m) => !m.usable && m.reachability === "unknown");
  const unreachable = models.filter(
    (m) => !m.usable && m.reachability !== "unknown",
  );
  const cloud = models.some(
    (m) => m.reachability === "cloud" || m.reachability === "unknown",
  );
  // A value the list does not carry still needs an option of its own, or the
  // <select> silently falls back to its first entry and the operator reads
  // "bot default" while a model IS set. Two ways to get here, both real: a
  // spec outside the curated set, and the window before the registry loads.
  const orphan = value !== "" && !selected;

  return (
    <div className="space-y-1">
      {custom ? (
        <div className="flex items-center gap-2">
          <Input
            id={id}
            size="sm"
            type="text"
            className="font-mono"
            placeholder="provider/model-id"
            value={value}
            disabled={disabled}
            onChange={(e) => onChange(e.currentTarget.value)}
          />
          <button
            type="button"
            className="shrink-0 text-caption text-fg-muted hover:text-fg-default underline"
            onClick={() => {
              setCustom(false);
              onChange("");
            }}
          >
            list
          </button>
        </div>
      ) : (
        <Select
          id={id}
          value={value}
          disabled={disabled}
          onChange={(e) => {
            const next = e.currentTarget.value;
            if (next === CUSTOM) {
              setCustom(true);
              onChange("");
              return;
            }
            onChange(next);
          }}
        >
          <option value="">{inheritLabel}</option>
          {orphan && <option value={value}>{value}</option>}
          {usable.length > 0 && (
            <optgroup
              label={
                cloud
                  ? "Available for this team's runs"
                  : "Available on this host"
              }
            >
              {usable.map((m) => (
                <option key={m.spec} value={m.spec}>
                  {optionLabel(m)}
                  {m.recommended ? " · recommended" : ""}
                </option>
              ))}
            </optgroup>
          )}
          {unproven.length > 0 && (
            <optgroup label="Not proven for this team's runs">
              {unproven.map((m) => (
                <option key={m.spec} value={m.spec}>
                  {optionLabel(m)}
                </option>
              ))}
            </optgroup>
          )}
          {unreachable.length > 0 && (
            // Listed, not hidden: seeing that a model exists but needs a
            // credential is what tells an operator which key to set. Selecting
            // one is allowed and warned about, never silently swallowed.
            <optgroup
              label={
                cloud
                  ? "Needs a credential this team does not have"
                  : "Needs a credential this host does not have"
              }
            >
              {unreachable.map((m) => (
                <option key={m.spec} value={m.spec}>
                  {optionLabel(m)}
                </option>
              ))}
            </optgroup>
          )}
          <optgroup label="Other">
            <option value={CUSTOM}>Custom…</option>
          </optgroup>
        </Select>
      )}

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

      {!compact && selected && (
        <p className="text-caption text-fg-subtle">
          {formatContextWindow(selected.context_window)} context ·{" "}
          {selected.tool_call ? "tools" : "no tools"} ·{" "}
          {selected.reasoning ? "reasoning" : "no reasoning"} ·{" "}
          {formatModelPrice(selected)}
          {selected.backends && selected.backends.length > 0
            ? ` · via ${selected.backends.join(", ")}`
            : ""}
        </p>
      )}

      {/* The recommended default is always one click away — the whole point of
          keeping it visible is that a cheaper pick can be undone without the
          operator having to remember what the good one was called. */}
      {recommended && value !== recommended.spec && (
        <button
          type="button"
          className="text-caption text-accent-fg hover:underline"
          disabled={disabled}
          onClick={() => {
            setCustom(false);
            onChange(recommended.spec);
          }}
        >
          Use recommended — {recommended.spec}
        </button>
      )}
    </div>
  );
}
