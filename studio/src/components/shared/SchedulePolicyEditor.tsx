import type { ScheduledBot } from "@/api/schedules";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";

// SchedulePolicyEditor edits one schedule's tick policy (pkg/schedgate):
// overlap behavior, optional concurrency cap, and the guard command whose
// stdout becomes vars[guard_var] on fire. Controlled + presentational —
// the host dialog owns the draft value and the Save action; validation of
// the merged row stays the server's (400 with a precise message).

/** Form-friendly draft of the schedgate policy (numbers kept as strings). */
export interface SchedulePolicyValue {
  overlap: "skip" | "allow";
  maxConcurrent: string;
  guard: string;
  guardTimeout: string;
  guardVar: string;
}

/** Seeds the draft from an existing schedule (or empty for creation). */
export function policyValueFromSchedule(
  s?: Pick<
    ScheduledBot,
    "overlap" | "max_concurrent" | "guard" | "guard_timeout" | "guard_var"
  >,
): SchedulePolicyValue {
  return {
    overlap: s?.overlap === "allow" ? "allow" : "skip",
    maxConcurrent: s?.max_concurrent ? String(s.max_concurrent) : "",
    guard: s?.guard ?? "",
    guardTimeout: s?.guard_timeout ?? "",
    guardVar: s?.guard_var ?? "",
  };
}

/** The policy fields of a create body / PATCH, built from the draft.
 *  max_concurrent only applies under overlap=allow; 0 clears the cap. */
export function policyFieldsFromValue(v: SchedulePolicyValue): {
  overlap: "skip" | "allow";
  max_concurrent: number;
  guard: string;
  guard_timeout: string;
  guard_var: string;
} {
  const n = Number(v.maxConcurrent);
  return {
    overlap: v.overlap,
    max_concurrent:
      v.overlap === "allow" && v.maxConcurrent.trim() !== "" && Number.isFinite(n)
        ? n
        : 0,
    guard: v.guard.trim(),
    guard_timeout: v.guardTimeout.trim(),
    guard_var: v.guardVar.trim(),
  };
}

export default function SchedulePolicyEditor({
  value,
  onChange,
  disabled = false,
}: {
  value: SchedulePolicyValue;
  onChange: (value: SchedulePolicyValue) => void;
  disabled?: boolean;
}) {
  const set = (patch: Partial<SchedulePolicyValue>) =>
    onChange({ ...value, ...patch });

  return (
    <div className="grid gap-2 text-xs sm:grid-cols-2">
      <label className="grid gap-1">
        <span className="text-fg-muted">Overlap</span>
        <Select
          value={value.overlap}
          disabled={disabled}
          onChange={(e) =>
            set({ overlap: e.currentTarget.value === "allow" ? "allow" : "skip" })
          }
        >
          <option value="skip">skip — pass the tick while a run is live (audited)</option>
          <option value="allow">allow — fire even with live runs</option>
        </Select>
      </label>
      <label className="grid gap-1">
        <span className="text-fg-muted">Max concurrent (allow only, 0 = unlimited)</span>
        <Input
          type="number"
          min={0}
          value={value.maxConcurrent}
          disabled={disabled || value.overlap !== "allow"}
          onChange={(e) => set({ maxConcurrent: e.currentTarget.value })}
          placeholder="0"
        />
      </label>
      <label className="grid gap-1 sm:col-span-2">
        <span className="text-fg-muted">
          Guard command — exit 0 fires the run (stdout becomes a var), non-zero skips it
        </span>
        <Input
          value={value.guard}
          disabled={disabled}
          onChange={(e) => set({ guard: e.currentTarget.value })}
          placeholder="e.g. gh api repos/o/r/pulls --jq 'length > 0'"
          className="font-mono"
        />
      </label>
      <label className="grid gap-1">
        <span className="text-fg-muted">Guard timeout</span>
        <Input
          value={value.guardTimeout}
          disabled={disabled}
          onChange={(e) => set({ guardTimeout: e.currentTarget.value })}
          placeholder="30s"
        />
      </label>
      <label className="grid gap-1">
        <span className="text-fg-muted">Guard var (stdout lands here)</span>
        <Input
          value={value.guardVar}
          disabled={disabled}
          onChange={(e) => set({ guardVar: e.currentTarget.value })}
          placeholder="guard_output"
          className="font-mono"
        />
      </label>
      <p className="text-fg-subtle sm:col-span-2">
        Tick decisions land on the team audit trail (actions schedule.tick.*).
      </p>
    </div>
  );
}
