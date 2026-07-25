import { Button } from "@/components/ui/Button";
import { FieldLabel } from "@/components/ui/FieldLabel";
import { Input } from "@/components/ui/Input";
import { humanizeCron } from "@/lib/humanizeCron";

// CronField is the canonical cron editor: a mono input with preset
// shortcuts and a live humanized preview (humanizeCron understands the
// CRON_TZ=<zone> prefix). Controlled + presentational — the host owns
// the value and any submit-time validation; the server stays the
// authority on cron correctness.

export const CRON_PRESETS = [
  { label: "Hourly", cron: "0 * * * *" },
  { label: "Daily 02:00", cron: "0 2 * * *" },
  { label: "Weekly Mon 02:00", cron: "0 2 * * 1" },
];

export default function CronField({
  value,
  onChange,
  disabled = false,
  hideLabel = false,
  ariaLabel,
}: {
  value: string;
  onChange: (value: string) => void;
  disabled?: boolean;
  /** Suppress the built-in FieldLabel when the host renders its own
   *  (e.g. a compact per-row caption) — pair with `ariaLabel`. */
  hideLabel?: boolean;
  /** Accessible name for the input when the visible label is hidden. */
  ariaLabel?: string;
}) {
  const human = humanizeCron(value);
  return (
    <div>
      {!hideLabel && (
        <FieldLabel>Cron (5-field, UTC — or prefix CRON_TZ=&lt;zone&gt;)</FieldLabel>
      )}
      <Input
        value={value}
        disabled={disabled}
        onChange={(e) => onChange(e.currentTarget.value)}
        placeholder="0 2 * * *"
        className="font-mono"
        aria-label={ariaLabel}
      />
      <div className="mt-1 flex flex-wrap items-center gap-1">
        {CRON_PRESETS.map((p) => (
          <Button
            key={p.cron}
            variant="ghost"
            size="sm"
            disabled={disabled}
            onClick={() => onChange(p.cron)}
          >
            {p.label}
          </Button>
        ))}
      </div>
      <p className="mt-1 min-h-4 text-xs text-fg-muted" aria-live="polite">
        {human ??
          (value.trim()
            ? "Unrecognized shape — the raw expression is used as-is."
            : "")}
      </p>
    </div>
  );
}
