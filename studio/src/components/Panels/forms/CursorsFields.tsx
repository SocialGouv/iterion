import { useCallback, useMemo } from "react";

import type { CursorBlock, CursorDecl, CursorSetting } from "@/api/types";
import { useDocumentStore } from "@/store/document";
import { CheckboxField, SelectField, TextField } from "./FormField";

interface Props {
  value: CursorBlock | undefined;
  onChange: (block: CursorBlock | undefined) => void;
}

/** Prompt-engineering cursor activations on agent/judge nodes.
 *
 *  Cursors are declared at workflow scope (`cursor <name>:`) and
 *  activated per-node here. The form lists every cursor declared in
 *  the current document and lets the operator pick an enum value
 *  (for `values:` cursors) or type a numeric / `${VAR}` value (for
 *  `bands:` cursors).
 *
 *  See docs/cursors.md for the design contract — cursors are
 *  framing dials, not gates. */
export default function CursorsFields({ value, onChange }: Props) {
  const document = useDocumentStore((s) => s.document);
  const declared: CursorDecl[] = document?.cursors ?? [];

  const settings: CursorSetting[] = value?.settings ?? [];
  const settingsByKey = useMemo(() => {
    const m = new Map<string, string>();
    for (const s of settings) m.set(s.key, s.value);
    return m;
  }, [settings]);

  const setEnabled = useCallback(
    (enabled: boolean) => {
      onChange({ enabled, settings: settings.length > 0 ? settings : undefined });
    },
    [onChange, settings],
  );

  const setValueFor = useCallback(
    (key: string, raw: string) => {
      const trimmed = raw.trim();
      if (trimmed === (settingsByKey.get(key) ?? "")) return;
      const filtered = settings.filter((s) => s.key !== key);
      const next: CursorSetting[] = trimmed
        ? [...filtered, { key, value: trimmed }]
        : filtered;
      next.sort((a, b) => a.key.localeCompare(b.key));
      onChange({
        enabled: value?.enabled ?? true,
        settings: next.length > 0 ? next : undefined,
      });
    },
    [onChange, settings, settingsByKey, value],
  );

  // Hide the section entirely when there are no declared cursors and
  // no settings to show — keeps the form uncluttered for workflows
  // that haven't adopted the feature yet.
  if (declared.length === 0 && settings.length === 0) {
    return null;
  }

  return (
    <details className="border-t border-border-default pt-2 mt-2" open={value !== undefined}>
      <summary className="cursor-pointer text-xs text-fg-subtle font-semibold mb-1">
        Cursors <span className="text-fg-subtle">(prompt calibration)</span>
      </summary>
      <div className="pl-2">
        <CheckboxField
          label="Enabled"
          checked={value?.enabled ?? false}
          onChange={setEnabled}
          help="When off, no calibration section is appended to the system prompt."
        />
        {declared.map((c) => {
          const current = settingsByKey.get(c.name) ?? "";
          if (c.values && c.values.length > 0) {
            const options = c.values.map((v) => ({ value: v.name, label: v.name }));
            return (
              <SelectField
                key={c.name}
                label={c.name}
                value={current}
                onChange={(v) => setValueFor(c.name, v)}
                options={[{ value: "", label: "(unset)" }, ...options]}
                help={c.description ?? "Enum cursor."}
              />
            );
          }
          // Numeric / hybrid: plain text input so 0..1 and ${VAR}
          // forms are both accepted.
          return (
            <TextField
              key={c.name}
              label={c.name}
              value={current}
              onChange={(v) => setValueFor(c.name, v)}
              placeholder="0.0..1.0 or ${VAR}"
            />
          );
        })}
        {/* Render settings whose cursor is no longer declared — gives
         *  the operator a chance to clear them without round-tripping
         *  through source. */}
        {settings
          .filter((s) => !declared.some((c) => c.name === s.key))
          .map((s) => (
            <TextField
              key={s.key}
              label={`${s.key} (unknown cursor)`}
              value={s.value}
              onChange={(v) => setValueFor(s.key, v)}
            />
          ))}
      </div>
    </details>
  );
}
