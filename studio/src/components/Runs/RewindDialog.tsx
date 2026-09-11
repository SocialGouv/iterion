import { useMemo, useState } from "react";

import { getRun, rewindRun, type RunHeader } from "@/api/runs";
import { Button, Checkbox, Dialog, Input } from "@/components/ui";
import { errorMessage } from "@/lib/errorHints";
import { useRunStore } from "@/store/run";
import { useUIStore } from "@/store/ui";

interface Props {
  run: RunHeader;
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onRewound: () => void;
}

function checkpointNodes(run: RunHeader): string[] {
  const outputs = run.checkpoint?.outputs;
  if (!outputs || typeof outputs !== "object" || Array.isArray(outputs)) {
    return [];
  }
  return Object.keys(outputs as Record<string, unknown>).sort();
}

// Rewind is the recovery bridge for a terminal DSL `fail`: it re-anchors the
// checkpoint, parks the run as cancelled, and only then hands the operator to
// the existing Resume confirmation. Workspace restoration stays off by
// default so recovering engine state cannot erase unrelated local edits.
export default function RewindDialog({
  run,
  open,
  onOpenChange,
  onRewound,
}: Props) {
  const candidates = useMemo(() => checkpointNodes(run), [run]);
  const [automatic, setAutomatic] = useState(false);
  const [nodeId, setNodeId] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const applySnapshot = useRunStore((state) => state.applySnapshot);
  const addToast = useUIStore((state) => state.addToast);

  const reset = () => {
    setAutomatic(false);
    setNodeId("");
    setBusy(false);
    setError(null);
  };

  const submit = async () => {
    const target = nodeId.trim();
    if (!automatic && !target) {
      setError("Choose a previously reached node, or enable automatic source targeting.");
      return;
    }
    setBusy(true);
    setError(null);
    try {
      const result = await rewindRun(run.id, {
        ...(automatic ? { auto: true } : { node_id: target }),
        restore_scope: "none",
      });
      const snapshot = await getRun(run.id);
      applySnapshot(snapshot);
      addToast(`Rewound to ${result.node_id}`, "success");
      reset();
      onOpenChange(false);
      onRewound();
    } catch (cause) {
      setError(errorMessage(cause));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (busy) return;
        if (!next) reset();
        onOpenChange(next);
      }}
      title="Recover failed run"
      description={
        <span>
          <span className="font-mono">{run.id}</span>
          {" — terminal, but rewindable"}
        </span>
      }
      widthClass="max-w-lg"
      footer={
        <>
          <Button
            variant="ghost"
            size="sm"
            onClick={() => onOpenChange(false)}
            disabled={busy}
          >
            Cancel
          </Button>
          <Button
            variant="danger"
            size="sm"
            onClick={() => void submit()}
            loading={busy}
            disabled={busy || (!automatic && !nodeId.trim())}
          >
            Rewind checkpoint
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3 text-sm">
        <p className="text-fg-muted">
          Invalidate this node and all downstream outputs, then continue with a
          separate Resume confirmation. Workspace files are left untouched.
        </p>

        <label className="flex flex-col gap-1">
          <span className="text-micro uppercase tracking-wide text-fg-subtle">
            Recovery node
          </span>
          <Input
            value={nodeId}
            onChange={(event) => setNodeId(event.target.value)}
            disabled={busy || automatic}
            list={`rewind-nodes-${run.id}`}
            placeholder="Previously reached node id"
            className="font-mono"
          />
          <datalist id={`rewind-nodes-${run.id}`}>
            {candidates.map((candidate) => (
              <option key={candidate} value={candidate} />
            ))}
          </datalist>
          <span className="text-caption text-fg-subtle">
            Pick the earliest node whose inputs or artifacts must be rebuilt.
          </span>
        </label>

        <label className="flex items-start gap-2 cursor-pointer select-none">
          <Checkbox
            checked={automatic}
            onChange={(event) => setAutomatic(event.target.checked)}
            disabled={busy}
            className="mt-0.5"
          />
          <span className="flex flex-col">
            <span className="font-medium">Target workflow source changes automatically</span>
            <span className="text-micro text-fg-subtle">
              Use this only after editing the bot. Data or artifact drift needs
              an explicit recovery node above.
            </span>
          </span>
        </label>

        {error && <div className="text-xs text-danger break-words">{error}</div>}
      </div>
    </Dialog>
  );
}
