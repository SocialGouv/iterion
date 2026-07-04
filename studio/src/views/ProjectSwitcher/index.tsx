import { useEffect, useMemo, useState } from "react";
import { TrashIcon } from "@radix-ui/react-icons";

import { Button, Dialog, IconButton, Input } from "@/components/ui";
import { useProjects } from "@/hooks/useProjects";
import { useDesktop } from "@/hooks/useDesktop";
import { desktop as desktopBridge, isCloudConnection, isDesktop } from "@/lib/desktopBridge";

import AddProjectDialog from "./AddProjectDialog";
import CloudConnectModal from "./CloudConnectModal";

interface Props {
  open: boolean;
  onClose: () => void;
}

export default function ProjectSwitcher({ open, onClose }: Props) {
  const {
    projects,
    currentProject,
    switchProject,
    addProject,
    removeProject,
  } = useProjects();
  // pickAndAddProject only exists in desktop mode (Wails native folder
  // picker). In web mode we open AddProjectDialog instead.
  const desktop = useDesktop();
  const [query, setQuery] = useState("");
  // Inline-confirm pattern: the trash button on a row sets that
  // project's id here, which swaps the row affordances to a Cancel /
  // Confirm pair. Previously we used a portaled ConfirmDialog
  // (createPortal to document.body), but Radix Dialog interprets
  // pointerdowns on portaled siblings as outside-clicks and closes
  // itself — which interrupted the async removeProject flow and left
  // the list out of sync.
  const [pendingRemovalId, setPendingRemovalId] = useState<string | null>(null);
  const [busyId, setBusyId] = useState<string | null>(null);
  const [addOpen, setAddOpen] = useState(false);
  const [cloudOpen, setCloudOpen] = useState(false);

  useEffect(() => {
    if (open) {
      setQuery("");
      setPendingRemovalId(null);
    }
  }, [open]);

  const filtered = useMemo(() => {
    if (!query.trim()) return projects;
    const q = query.toLowerCase();
    return projects.filter(
      (p) =>
        p.name.toLowerCase().includes(q) ||
        (p.dir ?? "").toLowerCase().includes(q) ||
        (p.cloud_url ?? "").toLowerCase().includes(q) ||
        (p.cloud_email ?? "").toLowerCase().includes(q),
    );
  }, [projects, query]);

  const onConfirmRemove = async (id: string) => {
    setBusyId(id);
    try {
      const target = projects.find((p) => p.id === id);
      // Cloud connections clear their keychain refresh token via
      // removeConnection; local projects use the shared removeProject.
      if (target && isCloudConnection(target) && isDesktop()) {
        await desktopBridge.removeConnection(id);
      } else {
        await removeProject(id);
      }
    } finally {
      setBusyId(null);
      setPendingRemovalId(null);
    }
  };

  const onAddClicked = async () => {
    if (isDesktop()) {
      const added = await desktop.pickAndAddProject();
      if (added) onClose();
      return;
    }
    setAddOpen(true);
  };

  return (
    <>
      <Dialog
        open={open}
        onOpenChange={(o) => !o && onClose()}
        title="Switch project"
        widthClass="max-w-xl"
      >
        <div className="flex flex-col gap-2">
          <Input
            autoFocus
            placeholder="Type a project name or path…"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            size="md"
          />
          <ul className="flex flex-col max-h-80 overflow-y-auto">
            {filtered.length === 0 && (
              <li className="px-2 py-6 text-center text-xs text-fg-subtle italic">
                No projects match “{query}”.
              </li>
            )}
            {filtered.map((p) => {
              const isCurrent = currentProject?.id === p.id;
              const isPending = pendingRemovalId === p.id;
              const isBusy = busyId === p.id;
              return (
                <li
                  key={p.id}
                  className="group relative flex items-center gap-2 pr-2 min-w-0"
                >
                  {/*
                    `min-w-0` on the flex-1 button is what lets the
                    inner truncate actually clip — without it the
                    button refuses to shrink below the project name's
                    intrinsic width, and the confirm-Remove buttons
                    get pushed past the right edge of the popover
                    (requiring the operator to scroll horizontally
                    to reach the dangerous click target). Pair with
                    `shrink-0` on the confirm cluster below so the
                    buttons keep their natural size and the name
                    takes the leftover space.
                  */}
                  <button
                    type="button"
                    className="flex-1 min-w-0 text-left pl-2 py-2 rounded hover:bg-surface-2 disabled:opacity-60 focus-visible:ring-1 focus-visible:ring-accent"
                    disabled={isPending || isBusy}
                    onClick={async () => {
                      await switchProject(p.id);
                      onClose();
                    }}
                  >
                    <div className="font-semibold flex items-center gap-2 truncate">
                      <span className="truncate">{p.name}</span>
                      {isCloudConnection(p) && (
                        <span className="text-caption uppercase tracking-wider text-fg-subtle border border-border-default rounded px-1 shrink-0">
                          cloud
                        </span>
                      )}
                      {isCurrent && (
                        <span className="text-caption uppercase tracking-wider text-accent-text shrink-0">
                          current
                        </span>
                      )}
                    </div>
                    <div className="text-xs text-fg-subtle truncate">
                      {isCloudConnection(p)
                        ? p.cloud_email || p.cloud_url
                        : p.dir}
                    </div>
                  </button>
                  {isPending ? (
                    <div className="flex items-center gap-1 text-micro shrink-0">
                      <span className="text-fg-subtle mr-1">Remove?</span>
                      <Button
                        size="sm"
                        variant="ghost"
                        onClick={() => setPendingRemovalId(null)}
                        disabled={isBusy}
                      >
                        Cancel
                      </Button>
                      <Button
                        size="sm"
                        variant="danger"
                        onClick={() => void onConfirmRemove(p.id)}
                        disabled={isBusy}
                      >
                        {isBusy ? "Removing…" : "Remove"}
                      </Button>
                    </div>
                  ) : (
                    <IconButton
                      label={`Remove ${p.name} from the list (folder kept on disk)`}
                      tooltip={`Remove "${p.name}" from the list (folder kept on disk)`}
                      size="sm"
                      variant="danger"
                      onClick={() => setPendingRemovalId(p.id)}
                      className="opacity-0 group-hover:opacity-100 focus:opacity-100 focus-visible:opacity-100 transition-opacity"
                    >
                      <TrashIcon />
                    </IconButton>
                  )}
                </li>
              );
            })}
          </ul>
          <div className="pt-2 border-t border-border-default flex items-center gap-2">
            <Button variant="ghost" size="sm" onClick={() => void onAddClicked()}>
              + Add project…
            </Button>
            {isDesktop() && (
              <Button
                variant="ghost"
                size="sm"
                onClick={() => setCloudOpen(true)}
              >
                Connect to Cloud…
              </Button>
            )}
          </div>
        </div>
      </Dialog>
      <AddProjectDialog
        open={addOpen}
        onClose={() => setAddOpen(false)}
        onAdd={async (dir) => {
          await addProject(dir);
          onClose();
        }}
      />
      <CloudConnectModal
        open={cloudOpen}
        onClose={() => setCloudOpen(false)}
        onConnected={() => {
          setCloudOpen(false);
          onClose();
        }}
      />
    </>
  );
}
