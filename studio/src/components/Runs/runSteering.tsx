import { useState } from "react";

import {
  bumpLoop,
  raiseBudget,
  type RaiseBudgetCaps,
} from "@/api/runSteering";
import type { RunStatus } from "@/api/runs";
import { Button, Dialog, Input } from "@/components/ui";
import { useUIStore } from "@/store/ui";

// Live-run steering dialogs (bump a loop's ceiling / raise budget
// caps), shared by the Overview meters. Both only make sense on a
// LIVE run — the engine applies the command at its next boundary.

export function isRunSteerable(status: RunStatus | undefined): boolean {
  return (
    status === "running" ||
    status === "paused_waiting_human" ||
    status === "paused_operator" ||
    status === "queued"
  );
}

interface BumpLoopDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  runId: string;
  loopName: string;
  currentMax?: number;
}

export function BumpLoopDialog({
  open,
  onOpenChange,
  runId,
  loopName,
  currentMax,
}: BumpLoopDialogProps) {
  const addToast = useUIStore((s) => s.addToast);
  const [delta, setDelta] = useState("2");
  const [busy, setBusy] = useState(false);

  const submit = async () => {
    const n = parseInt(delta, 10);
    if (!Number.isFinite(n) || n < 1) {
      addToast("Grant must be a positive number of iterations", "error");
      return;
    }
    setBusy(true);
    try {
      const res = await bumpLoop(runId, loopName, n);
      if (res.warning) {
        addToast(`Loop bumped, with a caveat: ${res.warning}`, "info");
      } else {
        addToast(
          `Loop "${loopName}" granted +${n} — ceiling now ${res.effective_max ?? "?"}`,
          "success",
        );
      }
      onOpenChange(false);
    } catch (err) {
      addToast(err instanceof Error ? err.message : String(err), "error");
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title={`Grant iterations — ${loopName}`}
      description={
        currentMax
          ? `Current ceiling: ${currentMax} iterations. The grant applies at the run's next step and survives resume.`
          : "The grant applies at the run's next step and survives resume."
      }
      widthClass="max-w-sm"
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button onClick={submit} disabled={busy}>
            {busy ? "Granting…" : "Grant"}
          </Button>
        </div>
      }
    >
      <label className="block text-sm">
        <span className="mb-1 block text-fg-muted">Extra iterations</span>
        <Input
          type="number"
          min={1}
          value={delta}
          onChange={(e) => setDelta(e.target.value)}
          autoFocus
        />
      </label>
    </Dialog>
  );
}

interface RaiseBudgetDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  runId: string;
  current: {
    maxCostUsd?: number | null;
    maxTokens?: number | null;
    maxIterations?: number | null;
    maxDuration?: string | null;
  };
}

export function RaiseBudgetDialog({
  open,
  onOpenChange,
  runId,
  current,
}: RaiseBudgetDialogProps) {
  const addToast = useUIStore((s) => s.addToast);
  const [costUsd, setCostUsd] = useState("");
  const [tokens, setTokens] = useState("");
  const [iterations, setIterations] = useState("");
  const [duration, setDuration] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = async () => {
    const caps: RaiseBudgetCaps = {};
    if (costUsd.trim() !== "") caps.max_cost_usd = parseFloat(costUsd);
    if (tokens.trim() !== "") caps.max_tokens = parseInt(tokens, 10);
    if (iterations.trim() !== "") caps.max_iterations = parseInt(iterations, 10);
    if (duration.trim() !== "") caps.max_duration = duration.trim();
    if (Object.keys(caps).length === 0) {
      addToast("Set at least one cap to raise", "error");
      return;
    }
    setBusy(true);
    try {
      const res = await raiseBudget(runId, caps);
      if (res.noop) {
        addToast(res.noop_reason || "New caps do not exceed the current ones", "info");
      } else if (res.warning) {
        addToast(`Caps raised, with a caveat: ${res.warning}`, "info");
      } else {
        addToast("Budget caps raised", "success");
      }
      onOpenChange(false);
    } catch (err) {
      addToast(err instanceof Error ? err.message : String(err), "error");
    } finally {
      setBusy(false);
    }
  };

  const field = (
    label: string,
    placeholder: string,
    value: string,
    set: (v: string) => void,
    type: "number" | "text" = "number",
  ) => (
    <label className="block text-sm">
      <span className="mb-1 block text-fg-muted">{label}</span>
      <Input
        type={type}
        placeholder={placeholder}
        value={value}
        onChange={(e) => set(e.target.value)}
      />
    </label>
  );

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title="Raise budget caps"
      description="Raise-only: values at or below the current caps are ignored. Leave a field empty to keep its cap. Applies at the run's next step and survives resume."
      widthClass="max-w-sm"
      footer={
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button onClick={submit} disabled={busy}>
            {busy ? "Raising…" : "Raise"}
          </Button>
        </div>
      }
    >
      <div className="space-y-3">
        {field(
          "Max cost (USD)",
          current.maxCostUsd != null ? `current ${current.maxCostUsd}` : "no cap",
          costUsd,
          setCostUsd,
        )}
        {field(
          "Max tokens",
          current.maxTokens != null ? `current ${current.maxTokens}` : "no cap",
          tokens,
          setTokens,
        )}
        {field(
          "Max iterations",
          current.maxIterations != null ? `current ${current.maxIterations}` : "no cap",
          iterations,
          setIterations,
        )}
        {field(
          "Max duration",
          current.maxDuration ? `current ${current.maxDuration}` : "e.g. 4h",
          duration,
          setDuration,
          "text",
        )}
      </div>
    </Dialog>
  );
}
