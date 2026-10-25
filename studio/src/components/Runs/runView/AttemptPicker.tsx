import { Select } from "@/components/ui/Select";
import { useRunAttempt } from "@/hooks/useRunAttempt";
import { useRunStore } from "@/store/run";

export function AttemptPicker() {
  const { attempts, attempt, isCurrent } = useRunAttempt();
  const select = useRunStore((s) => s.setSelectedAttemptID);
  if (!attempt) return null;
  return <label className="flex items-center gap-2 px-3 py-1 text-xs text-fg-muted">
    Attempt
    <Select fit aria-label="Execution attempt" value={isCurrent ? "current" : attempt.id}
      onChange={(e) => select(e.target.value === "current" ? null : e.target.value)}>
      <option value="current">{attempts.length} · Current</option>
      {attempts.slice(0, -1).map((a, i) => <option key={a.id} value={a.id}>
        {i + 1} · {new Date(a.started_at).toLocaleString()} · {a.workflow_hash?.slice(0, 8)}
      </option>)}
    </Select>
    <span>{isCurrent ? "Metrics for this attempt" : "Historical attempt"}</span>
  </label>;
}
