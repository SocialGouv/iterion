import { useEffect, useMemo, useState } from "react";

import { Button, Dialog, Input } from "@/components/ui";

import { useDesktop } from "@/hooks/useDesktop";

interface Props {
  open: boolean;
  onClose: () => void;
}

export default function ProjectSwitcher({ open, onClose }: Props) {
  const { projects, switchProject, pickAndAddProject } = useDesktop();
  const [query, setQuery] = useState("");

  useEffect(() => {
    if (open) setQuery("");
  }, [open]);

  const filtered = useMemo(() => {
    if (!query.trim()) return projects;
    const q = query.toLowerCase();
    return projects.filter(
      (p) =>
        p.name.toLowerCase().includes(q) || p.dir.toLowerCase().includes(q),
    );
  }, [projects, query]);

  return (
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
          {filtered.map((p) => (
            <li key={p.id}>
              <button
                className="w-full text-left px-2 py-2 rounded hover:bg-surface-2"
                onClick={async () => {
                  await switchProject(p.id);
                  onClose();
                }}
              >
                <div className="font-semibold">{p.name}</div>
                <div className="text-xs text-fg-subtle truncate">{p.dir}</div>
              </button>
            </li>
          ))}
        </ul>
        <div className="pt-2 border-t border-border-default">
          <Button
            variant="ghost"
            size="sm"
            onClick={async () => {
              const added = await pickAndAddProject();
              if (added) onClose();
            }}
          >
            + Add project…
          </Button>
        </div>
      </div>
    </Dialog>
  );
}
