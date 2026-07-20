import { Button, Card, FieldLabel, Input, Textarea } from "@/components/ui";

import type { EditableField } from "./fieldModel";

const httpUrlPattern = /^https?:\/\/\S+$/i;

// ---------------------------------------------------------------------------
// Field widgets — a plain textarea for strings, an add/remove list for arrays.
// ---------------------------------------------------------------------------

export function StringField({
  field,
  value,
  disabled,
  onChange,
}: {
  field: EditableField;
  value: string;
  disabled: boolean;
  onChange: (v: string) => void;
}) {
  const multiline = value.includes("\n") || value.length > 80;
  return (
    <Card>
      <FieldLabel help={field.parentLabel || undefined}>{field.leaf}</FieldLabel>
      <Textarea
        value={value}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value)}
        rows={multiline ? 8 : 3}
        className="font-mono"
      />
    </Card>
  );
}

export function ArrayField({
  field,
  value,
  disabled,
  onChange,
}: {
  field: EditableField;
  value: string[];
  disabled: boolean;
  onChange: (v: string[]) => void;
}) {
  // Feed lists are the common case and get url-typed inputs + validation;
  // any other array leaf falls back to plain text rows.
  const isFeeds = field.leaf === "feeds";
  const rows = value.length > 0 ? value : [""];
  const setAt = (i: number, v: string) => {
    const next = rows.slice();
    next[i] = v;
    onChange(next);
  };
  const removeAt = (i: number) => {
    const next = rows.slice();
    next.splice(i, 1);
    if (next.length === 0) next.push("");
    onChange(next);
  };
  const count = rows.filter((r) => r.trim().length > 0).length;
  return (
    <Card>
      <div className="mb-2 flex items-baseline justify-between">
        <FieldLabel help={field.parentLabel || undefined}>{field.leaf}</FieldLabel>
        <span className="text-caption text-fg-subtle">
          {count} item{count === 1 ? "" : "s"}
        </span>
      </div>
      <ul className="flex flex-col gap-2">
        {rows.map((item, i) => {
          const trimmed = item.trim();
          const err = isFeeds && trimmed.length > 0 && !httpUrlPattern.test(trimmed);
          return (
            <li key={i} className="flex items-center gap-2">
              <Input
                type={isFeeds ? "url" : "text"}
                inputMode={isFeeds ? "url" : undefined}
                autoComplete="off"
                spellCheck={false}
                size="md"
                value={item}
                error={err}
                disabled={disabled}
                placeholder={isFeeds ? "https://example.org/feed.xml" : ""}
                aria-label={`${field.leaf} ${i + 1}`}
                onChange={(e) => setAt(i, e.target.value)}
                className="font-mono"
              />
              <Button
                variant="ghost"
                size="sm"
                disabled={disabled}
                onClick={() => removeAt(i)}
                aria-label={`Remove ${field.leaf} ${i + 1}`}
              >
                Remove
              </Button>
            </li>
          );
        })}
      </ul>
      <div className="mt-2">
        <Button
          variant="secondary"
          size="sm"
          disabled={disabled}
          onClick={() => onChange([...rows, ""])}
        >
          + Add
        </Button>
      </div>
    </Card>
  );
}
