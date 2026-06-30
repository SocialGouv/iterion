import { useState } from "react";

import { setPluginConfig, type PluginConfigField, type PluginView } from "@/api/plugins";
import { useUIStore } from "@/store/ui";
import { Button } from "@/components/ui/Button";
import { Checkbox } from "@/components/ui/Checkbox";
import { Input } from "@/components/ui/Input";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Select } from "@/components/ui/Select";

// PluginConfigForm renders a plugin's declared config fields (like a Firefox
// add-on's preferences pane) and persists the values. Secret fields are never
// pre-filled — the server reports only whether one is set, and a blank
// submission keeps the prior value.
export default function PluginConfigForm({
  plugin,
  onSaved,
}: {
  plugin: PluginView;
  onSaved: () => void;
}) {
  const schema = plugin.config_schema ?? [];
  const addToast = useUIStore((s) => s.addToast);

  const [baseline, setBaseline] = useState(() => buildDraft(plugin));
  const [draft, setDraft] = useState(baseline);
  const [saving, setSaving] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const dirty = JSON.stringify(draft) !== JSON.stringify(baseline);
  const set = (key: string, value: string) => setDraft((d) => ({ ...d, [key]: value }));

  const save = async () => {
    setSaving(true);
    setErr(null);
    try {
      const updated = await setPluginConfig(plugin.name, draft);
      const next = buildDraft(updated);
      setBaseline(next);
      setDraft(next);
      addToast(`Saved ${plugin.name} settings`, "success");
      onSaved();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  };

  if (schema.length === 0) return null;

  const secretSet = new Set(plugin.config_secret_set ?? []);

  return (
    <div className="space-y-3">
      {err && (
        <InlineBanner tone="danger" layout="inline">
          {err}
        </InlineBanner>
      )}
      <div className="space-y-3">
        {schema.map((f) => (
          <ConfigFieldRow
            key={f.key}
            field={f}
            value={draft[f.key] ?? ""}
            secretSet={secretSet.has(f.key)}
            onChange={(v) => set(f.key, v)}
          />
        ))}
      </div>
      <div className="flex items-center gap-2">
        <Button variant="primary" size="sm" loading={saving} disabled={!dirty} onClick={() => void save()}>
          Save settings
        </Button>
        <Button
          variant="ghost"
          size="sm"
          disabled={!dirty || saving}
          onClick={() => setDraft(baseline)}
        >
          Reset
        </Button>
      </div>
    </div>
  );
}

// buildDraft seeds the editable values from a view: non-secret fields take their
// current value (or manifest default); secret fields always start blank.
function buildDraft(p: PluginView): Record<string, string> {
  const out: Record<string, string> = {};
  for (const f of p.config_schema ?? []) {
    out[f.key] = f.type === "secret" ? "" : (p.config_values?.[f.key] ?? f.default ?? "");
  }
  return out;
}

function ConfigFieldRow({
  field,
  value,
  secretSet,
  onChange,
}: {
  field: PluginConfigField;
  value: string;
  secretSet: boolean;
  onChange: (v: string) => void;
}) {
  const label = field.label || field.key;
  const help = field.description;

  // bool renders as a single checkbox row carrying its own label.
  if (field.type === "bool") {
    return (
      <div>
        <Checkbox
          label={label}
          checked={value === "true"}
          onChange={(e) => onChange(e.target.checked ? "true" : "false")}
        />
        {help && <p className="ml-6 mt-0.5 text-caption text-fg-subtle">{help}</p>}
      </div>
    );
  }

  return (
    <div className="space-y-1">
      <label className="flex items-center gap-1.5 text-xs font-medium text-fg-default">
        <span className="font-mono">{field.key}</span>
        {field.required && <span className="text-danger">*</span>}
      </label>
      {field.type === "enum" ? (
        <Select size="md" value={value} onChange={(e) => onChange(e.target.value)}>
          {!field.required && <option value="">—</option>}
          {(field.options ?? []).map((o) => (
            <option key={o} value={o}>
              {o}
            </option>
          ))}
        </Select>
      ) : (
        <Input
          size="md"
          type={field.type === "secret" ? "password" : field.type === "int" || field.type === "float" ? "number" : "text"}
          step={field.type === "float" ? "any" : undefined}
          value={value}
          placeholder={
            field.type === "secret"
              ? secretSet
                ? "•••••••• (set — leave blank to keep)"
                : "enter a value"
              : field.default
                ? `default: ${field.default}`
                : ""
          }
          onChange={(e) => onChange(e.target.value)}
        />
      )}
      {help && <p className="text-caption text-fg-subtle">{help}</p>}
    </div>
  );
}
