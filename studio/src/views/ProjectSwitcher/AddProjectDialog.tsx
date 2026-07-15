import { errorMessage } from "@/lib/errorHints";
import { useEffect, useMemo, useState } from "react";
import { ChevronLeftIcon, OpenInNewWindowIcon } from "@radix-ui/react-icons";

import { Button, Dialog, Input, Spinner } from "@/components/ui";
import { listFilesystem, type FilesystemListing } from "@/api/projects";
import { useServerInfoStore } from "@/store/serverInfo";
import { ErrorNotice } from "@/components/shared/ErrorNotice";
import { useAsyncAction } from "@/hooks/useAsyncAction";

interface Props {
  open: boolean;
  onClose: () => void;
  onAdd: (dir: string) => Promise<void>;
}

// AddProjectDialog is the web-mode equivalent of the desktop's native
// folder picker. The user can either type an absolute path directly
// or, when ITERION_BROWSE_ROOT is configured on the server, drill
// into a sandboxed directory tree via the Browse sub-panel.
export default function AddProjectDialog({ open, onClose, onAdd }: Props) {
  const serverInfo = useServerInfoStore((s) => s.info);
  const browseEnabled = !!serverInfo?.browse_root;

  const [path, setPath] = useState("");
  const { busy, error, run, setError, clearError } = useAsyncAction();
  const [browsing, setBrowsing] = useState(false);

  useEffect(() => {
    if (open) {
      setPath("");
      clearError();
      setBrowsing(false);
    }
  }, [open, clearError]);

  const confirm = () => {
    const trimmed = path.trim();
    if (!trimmed) {
      setError("Path is required");
      return;
    }
    return run(async () => {
      await onAdd(trimmed);
      onClose();
    });
  };

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => !o && onClose()}
      title="Add project"
      widthClass="max-w-xl"
    >
      <div className="flex flex-col gap-3">
        {browsing && browseEnabled ? (
          <BrowsePanel
            initialPath="/"
            onPick={(absDir) => {
              setPath(absDir);
              setBrowsing(false);
            }}
            onBack={() => setBrowsing(false)}
          />
        ) : (
          <>
            <label className="text-micro text-fg-muted">
              Absolute path to an iterion project folder
            </label>
            <Input
              autoFocus
              value={path}
              onChange={(e) => setPath(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter" && !busy) void confirm();
              }}
              placeholder="/path/to/your/project"
              size="md"
              disabled={busy}
            />
            {browseEnabled && (
              <Button
                variant="ghost"
                size="sm"
                onClick={() => setBrowsing(true)}
                disabled={busy}
              >
                <OpenInNewWindowIcon className="mr-1" /> Browse
                <span className="ml-2 text-caption text-fg-subtle">
                  under {serverInfo?.browse_root}
                </span>
              </Button>
            )}
            {error && <ErrorNotice error={error} />}
            <div className="flex items-center justify-end gap-2 pt-1">
              <Button variant="ghost" size="sm" onClick={onClose} disabled={busy}>
                Cancel
              </Button>
              <Button
                variant="primary"
                size="sm"
                onClick={() => void confirm()}
                disabled={busy || path.trim() === ""}
              >
                {busy ? "Adding…" : "Add project"}
              </Button>
            </div>
          </>
        )}
      </div>
    </Dialog>
  );
}

interface BrowsePanelProps {
  initialPath: string;
  onPick: (absDir: string) => void;
  onBack: () => void;
}

function BrowsePanel({ initialPath, onPick, onBack }: BrowsePanelProps) {
  const [cwd, setCwd] = useState(initialPath);
  // absHere is the resolved, server-side absolute path that
  // corresponds to `cwd`. We track it separately so the picker hands
  // back exactly what the server resolved (no client-side path
  // joining that could double-slash or diverge from symlink
  // resolution).
  const [absHere, setAbsHere] = useState<string | null>(null);
  const [listing, setListing] = useState<FilesystemListing | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    const ctrl = new AbortController();
    setLoading(true);
    setError(null);
    listFilesystem(cwd, ctrl.signal)
      .then((data) => {
        setListing(data);
        const root = data.root.replace(/\/$/, "");
        setAbsHere(data.cwd === "/" ? root : root + data.cwd);
        setLoading(false);
      })
      .catch((err) => {
        if (ctrl.signal.aborted) return;
        setError(errorMessage(err));
        setLoading(false);
      });
    return () => {
      ctrl.abort();
    };
  }, [cwd]);

  // Build a clickable breadcrumb from `cwd`. "/" → just the root chip.
  const crumbs = useMemo(() => {
    const parts = cwd.split("/").filter(Boolean);
    const out: { label: string; path: string }[] = [{ label: "root", path: "/" }];
    let acc = "";
    for (const p of parts) {
      acc += "/" + p;
      out.push({ label: p, path: acc });
    }
    return out;
  }, [cwd]);

  const pickHere = () => {
    if (absHere) onPick(absHere);
  };

  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-center gap-2">
        <Button variant="ghost" size="sm" onClick={onBack}>
          <ChevronLeftIcon className="mr-1" /> Back
        </Button>
        <div className="flex-1 overflow-x-auto whitespace-nowrap text-micro text-fg-subtle">
          {crumbs.map((c, idx) => (
            <span key={c.path}>
              {idx > 0 && <span className="mx-0.5">/</span>}
              <button
                type="button"
                className="hover:text-fg-default"
                onClick={() => setCwd(c.path)}
              >
                {c.label}
              </button>
            </span>
          ))}
        </div>
      </div>
      <div className="max-h-72 overflow-y-auto border border-border-default rounded bg-surface-1">
        {loading && (
          <div className="flex items-center gap-1.5 px-3 py-2 text-micro text-fg-subtle">
            <Spinner size="xs" /> Loading…
          </div>
        )}
        {error && <ErrorNotice error={error} />}
        {listing && !loading && listing.entries.length === 0 && (
          <div className="px-3 py-2 text-micro text-fg-subtle italic">
            No sub-directories here.
          </div>
        )}
        {listing && !loading && (
          <ul>
            {listing.entries.map((e) => (
              <li key={e.abs_dir}>
                <button
                  type="button"
                  className="w-full text-left px-3 py-1.5 text-body hover:bg-surface-2"
                  onClick={() => setCwd(cwd === "/" ? "/" + e.name : cwd + "/" + e.name)}
                >
                  {e.name}/
                </button>
              </li>
            ))}
          </ul>
        )}
      </div>
      <div className="flex items-center justify-end gap-2">
        <Button variant="primary" size="sm" onClick={pickHere} disabled={!listing}>
          Use this folder
        </Button>
      </div>
    </div>
  );
}
