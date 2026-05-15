import { useEffect, useMemo, useState } from "react";
import { TrashIcon } from "@radix-ui/react-icons";

import { Button, Dialog, Input } from "@/components/ui";
import { useProjects } from "@/hooks/useProjects";
import { useDesktop } from "@/hooks/useDesktop";
import { isDesktop } from "@/lib/desktopBridge";

import AddProjectDialog from "./AddProjectDialog";

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
        p.name.toLowerCase().includes(q) || p.dir.toLowerCase().includes(q),
    );
  }, [projects, query]);

  const onConfirmRemove = async (id: string) => {
    setBusyId(id);
    try {
      await removeProject(id);
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
            {filtered.map((p) => {
              const isCurrent = currentProject?.id === p.id;
              const isPending = pendingRemovalId === p.id;
              const isBusy = busyId === p.id;
              return (
                <li
                  key={p.id}
                  className="group relative flex items-center gap-2 pr-2"
                >
                  <button
                    className="flex-1 text-left pl-2 py-2 rounded hover:bg-surface-2 disabled:opacity-60"
                    disabled={isPending || isBusy}
                    onClick={async () => {
                      await switchProject(p.id);
                      onClose();
                    }}
                  >
                    <div className="font-semibold flex items-center gap-2">
                      {p.name}
                      {isCurrent && (
                        <span className="text-[10px] uppercase tracking-wider text-accent">
                          current
                        </span>
                      )}
                    </div>
                    <div className="text-xs text-fg-subtle truncate">
                      {p.dir}
                    </div>
                  </button>
                  {isPending ? (
                    <div className="flex items-center gap-1 text-[11px]">
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
                    <button
                      type="button"
                      className="p-1.5 rounded opacity-0 group-hover:opacity-100 hover:bg-danger-soft hover:text-danger focus:opacity-100 transition-opacity"
                      title={`Remove "${p.name}" from the list (folder kept on disk)`}
                      onClick={() => setPendingRemovalId(p.id)}
                    >
                      <TrashIcon />
                    </button>
                  )}
                </li>
              );
            })}
          </ul>
          <div className="pt-2 border-t border-border-default">
            <Button variant="ghost" size="sm" onClick={() => void onAddClicked()}>
              + Add project…
            </Button>
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
    </>
  );
}
