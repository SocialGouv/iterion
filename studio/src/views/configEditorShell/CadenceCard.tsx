import { useEffect, useRef, useState } from "react";

import { errorMessage } from "@/lib/errorHints";
import { getEditorSchedule, patchEditorSchedule, type EditorShare } from "@/api/configEditor";
import { Button, Card, FieldLabel, Input } from "@/components/ui";

import type { SaveStatus } from "./ShareEditor";

// ---------------------------------------------------------------------------
// CadenceCard — edit the cron of the schedule bound to this share's category.
// The recurrence lives in iterion's schedule store (visible in the Schedules
// view), NOT the repo config. Self-hides when the category has no schedule or
// the server has no scheduler (local mode) — it never breaks the content editor.
// ---------------------------------------------------------------------------

const CRON_PRESETS: { label: string; expr: string }[] = [
  { label: "Daily 08:00", expr: "0 8 * * *" },
  { label: "Weekdays 08:00", expr: "0 8 * * 1-5" },
  { label: "Weekly · Mon 08:00", expr: "0 8 * * 1" },
  { label: "Weekly · Wed 08:00", expr: "0 8 * * 3" },
];

// splitCronTZ preserves an optional "CRON_TZ=…" prefix so a preset only rewrites
// the schedule fields, keeping the timezone the operator set on the schedule.
function splitCronTZ(cron: string): { tz: string; expr: string } {
  const m = cron.match(/^(CRON_TZ=\S+\s+)([\s\S]*)$/);
  return m ? { tz: m[1] ?? "", expr: (m[2] ?? "").trim() } : { tz: "", expr: cron.trim() };
}

function formatNextFire(iso?: string): string | null {
  if (!iso) return null;
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? null : d.toLocaleString();
}

export function CadenceCard({
  teamID,
  share,
  readOnly,
}: {
  teamID: string;
  share: EditorShare;
  readOnly: boolean;
}) {
  const [loaded, setLoaded] = useState(false);
  const [hidden, setHidden] = useState(false);
  const [cron, setCron] = useState("");
  const [baseline, setBaseline] = useState("");
  const [nextFire, setNextFire] = useState<string | undefined>(undefined);
  const [saving, setSaving] = useState(false);
  const [status, setStatus] = useState<SaveStatus>({ kind: "idle" });
  const bootRef = useRef(false);

  useEffect(() => {
    if (bootRef.current) return;
    bootRef.current = true;
    void (async () => {
      try {
        const sched = await getEditorSchedule(teamID, share.id);
        if (!sched.exists || !sched.cron) {
          setHidden(true);
          return;
        }
        setCron(sched.cron);
        setBaseline(sched.cron);
        setNextFire(sched.next_fire_at);
        setLoaded(true);
      } catch {
        // No scheduler on this server, or the schedule read failed: the cadence
        // simply isn't editable here — hide the card, never break the editor.
        setHidden(true);
      }
    })();
  }, [teamID, share.id]);

  const dirty = cron.trim() !== baseline.trim();

  const setPreset = (expr: string) => {
    setCron(splitCronTZ(cron).tz + expr);
    setStatus({ kind: "idle" });
  };

  const onSave = async () => {
    const c = cron.trim();
    if (!c || c === baseline.trim()) return;
    setSaving(true);
    setStatus({ kind: "idle" });
    try {
      const r = await patchEditorSchedule(teamID, share.id, c);
      setCron(r.cron);
      setBaseline(r.cron);
      setNextFire(r.next_fire_at);
      setStatus({ kind: "saved", changed: 1 });
    } catch (err) {
      setStatus({ kind: "error", message: errorMessage(err) });
    } finally {
      setSaving(false);
    }
  };

  if (hidden || !loaded) return null;

  const next = formatNextFire(nextFire);
  return (
    <Card>
      <FieldLabel help="How often the digest is published — kept in the Schedules view">
        Cadence
      </FieldLabel>
      {!readOnly && (
        <div className="mb-2 flex flex-wrap gap-1.5">
          {CRON_PRESETS.map((p) => (
            <Button key={p.expr} variant="secondary" size="sm" onClick={() => setPreset(p.expr)}>
              {p.label}
            </Button>
          ))}
        </div>
      )}
      <Input
        value={cron}
        disabled={readOnly}
        onChange={(e) => {
          setCron(e.target.value);
          setStatus({ kind: "idle" });
        }}
        spellCheck={false}
        autoComplete="off"
        aria-label="Cron expression"
        className="font-mono"
      />
      <div className="mt-1.5 flex flex-wrap items-center justify-between gap-2">
        <span className="text-caption text-fg-subtle">
          {next ? `Next run: ${next}` : "cron expression, e.g. 0 8 * * 1"}
        </span>
        {!readOnly && (
          <div className="flex items-center gap-2">
            {status.kind === "saved" && (
              <span className="text-xs text-success-fg">Cadence saved</span>
            )}
            {status.kind === "error" && (
              <span className="text-xs text-danger-fg">{status.message}</span>
            )}
            <Button
              variant="secondary"
              size="sm"
              loading={saving}
              disabled={saving || !dirty}
              onClick={() => void onSave()}
            >
              Save cadence
            </Button>
          </div>
        )}
      </div>
    </Card>
  );
}
