import { useEffect, useMemo, useState } from "react";
import { useLocation } from "wouter";

import { forkRun } from "@/api/runs";
import type { RunHeader } from "@/api/runs";
import { Button, Dialog } from "@/components/ui";
import { useTabsStore } from "@/store/tabs";
import { useUIStore } from "@/store/ui";

interface Props {
  run: RunHeader;
  // Initial anchor for the fork. The dialog presents this read-only —
  // the user picks the (node, turn) via the per-step Fork-from-here
  // buttons in the NodeDetailPanel; the dialog itself is for confirming
  // the rewind / inputs / name choices.
  anchor: { nodeId: string; turnIndex: number } | null;
  open: boolean;
  onOpenChange: (open: boolean) => void;
}

// ForkDialog confirms a fork request: rewind-code checkbox, optional
// new-inputs JSON, optional friendly name. Submit POSTs /fork and
// (by default) opens a new tab on the returned run id.
export default function ForkDialog({ run, anchor, open, onOpenChange }: Props) {
  const [rewindCode, setRewindCode] = useState(false);
  const [forkName, setForkName] = useState("");
  const [newInputs, setNewInputs] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [, setLocation] = useLocation();
  const openTab = useTabsStore((s) => s.openTab);
  const setActive = useTabsStore((s) => s.setActive);
  const addToast = useUIStore((s) => s.addToast);

  const parentInputsJSON = useMemo(
    () => JSON.stringify((run as unknown as { inputs?: Record<string, unknown> }).inputs ?? {}, null, 2),
    [run],
  );

  useEffect(() => {
    if (open) {
      setRewindCode(false);
      setForkName("");
      setNewInputs(parentInputsJSON);
      setError(null);
      setBusy(false);
    }
  }, [open, run.id, parentInputsJSON]);

  const submit = async (e?: React.MouseEvent) => {
    if (!anchor) {
      setError("No fork anchor selected — click a turn's Fork glyph first.");
      return;
    }
    setBusy(true);
    setError(null);
    try {
      let parsedInputs: Record<string, unknown> | undefined;
      if (newInputs.trim() && newInputs.trim() !== parentInputsJSON.trim()) {
        try {
          parsedInputs = JSON.parse(newInputs);
        } catch (jerr) {
          throw new Error(
            `Invalid JSON in new inputs: ${jerr instanceof Error ? jerr.message : String(jerr)}`,
          );
        }
      }
      const resp = await forkRun(run.id, {
        node_id: anchor.nodeId,
        turn_index: anchor.turnIndex,
        rewind_code: rewindCode,
        fork_name: forkName || undefined,
        new_inputs: parsedInputs,
      });
      // Open a tab on the new run; Shift+click submit keeps the parent
      // tab focused (background fork).
      const background = Boolean(e?.shiftKey);
      const tabId = openTab("run", { runId: resp.new_run_id });
      if (!background) {
        setActive(tabId);
        setLocation(`/runs/${encodeURIComponent(resp.new_run_id)}`);
      }
      addToast(
        background
          ? `Forked in background → ${resp.new_run_id}`
          : `Forked → ${resp.new_run_id}`,
        "success",
      );
      onOpenChange(false);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        if (busy) return;
        onOpenChange(o);
      }}
      title="Fork run"
      description={
        anchor ? (
          <span>
            Anchor: <span className="font-mono">{anchor.nodeId}</span>
            {" · turn "}
            <span className="font-mono">{anchor.turnIndex}</span>
          </span>
        ) : (
          <span className="text-fg-subtle">No anchor selected</span>
        )
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
            variant="primary"
            size="sm"
            onClick={(e) => void submit(e)}
            loading={busy}
            disabled={busy || !anchor}
            title="Shift+click to fork without switching to the new tab"
          >
            Fork →
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3 text-sm">
        <p className="text-fg-muted text-[11px]">
          Forking creates a new run that re-executes from the chosen anchor with
          the parent's conversation/session restored. The new run starts in{" "}
          <span className="font-mono">cancelled</span> status — click Resume on
          it to actually start.
        </p>

        <label className="flex flex-col gap-1">
          <span className="text-[11px] uppercase tracking-wide text-fg-subtle">
            Fork name
          </span>
          <input
            type="text"
            value={forkName}
            onChange={(ev) => setForkName(ev.target.value)}
            disabled={busy}
            placeholder={`${run.name || run.workflow_name} · fork @ ${anchor?.nodeId ?? "…"}`}
            className="rounded border border-border-default bg-surface-1 px-2 py-1 font-mono text-xs"
          />
        </label>

        <label className="flex items-start gap-2 cursor-pointer select-none">
          <input
            type="checkbox"
            checked={rewindCode}
            onChange={(ev) => setRewindCode(ev.target.checked)}
            disabled={busy}
            className="mt-0.5 accent-accent"
          />
          <span className="flex flex-col">
            <span className="font-medium">Rewind worktree</span>
            <span className="text-[11px] text-fg-subtle">
              Reset the new worktree to the git snapshot captured at this node
              boundary. Off (default) inherits the parent's current files —
              useful when you want to retry the LLM step against fixes you
              made downstream.
            </span>
          </span>
        </label>

        <label className="flex flex-col gap-1">
          <span className="text-[11px] uppercase tracking-wide text-fg-subtle">
            New inputs (JSON)
          </span>
          <textarea
            value={newInputs}
            onChange={(ev) => setNewInputs(ev.target.value)}
            disabled={busy}
            spellCheck={false}
            rows={6}
            className="rounded border border-border-default bg-surface-1 px-2 py-1 font-mono text-xs leading-relaxed"
          />
          <span className="text-[10px] text-fg-subtle">
            Pre-filled with the parent's inputs. Edits are merged onto the
            parent's map on the new run.
          </span>
        </label>

        {error && (
          <div className="text-xs text-danger break-words">{error}</div>
        )}
      </div>
    </Dialog>
  );
}
