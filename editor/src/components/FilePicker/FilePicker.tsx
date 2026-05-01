import { useEffect, useMemo, useState } from "react";
import * as api from "@/api/client";
import type { FileEntry } from "@/api/types";
import { useRecentsStore } from "@/store/recents";
import { Dialog, Tabs, Input } from "@/components/ui";
import { MagnifyingGlassIcon, FileIcon, ClockIcon, RocketIcon, TrashIcon } from "@radix-ui/react-icons";

interface FilePickerProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Called with the chosen file path (workspace-relative) or example name. */
  onPick: (kind: "file" | "example", path: string) => void;
}

export default function FilePicker({ open, onOpenChange, onPick }: FilePickerProps) {
  const [tab, setTab] = useState<string>("recents");
  const [files, setFiles] = useState<FileEntry[]>([]);
  const [examples, setExamples] = useState<string[]>([]);
  const [query, setQuery] = useState("");
  const recents = useRecentsStore((s) => s.recents);
  const removeRecent = useRecentsStore((s) => s.removeRecent);
  const clearRecents = useRecentsStore((s) => s.clearRecents);

  useEffect(() => {
    if (!open) return;
    api.listFiles().then(setFiles).catch(() => setFiles([]));
    api.listExamples().then(setExamples).catch(() => setExamples([]));
    setQuery("");
    setTab(recents.length > 0 ? "recents" : "files");
  }, [open, recents.length]);

  const filteredFiles = useMemo(() => {
    if (!query) return files;
    const q = query.toLowerCase();
    return files.filter((f) => f.name.toLowerCase().includes(q));
  }, [files, query]);

  const filteredExamples = useMemo(() => {
    if (!query) return examples;
    const q = query.toLowerCase();
    return examples.filter((n) => n.toLowerCase().includes(q));
  }, [examples, query]);

  const filteredRecents = useMemo(() => {
    if (!query) return recents;
    const q = query.toLowerCase();
    return recents.filter((p) => p.toLowerCase().includes(q));
  }, [recents, query]);

  const pick = (kind: "file" | "example", path: string) => {
    onPick(kind, path);
    onOpenChange(false);
  };

  return (
    <Dialog
      open={open}
      onOpenChange={onOpenChange}
      title="Open workflow"
      widthClass="max-w-xl"
    >
      <div className="space-y-3">
        <Input
          autoFocus
          placeholder="Search files..."
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          leadingIcon={<MagnifyingGlassIcon />}
          size="md"
        />
        <Tabs
          value={tab}
          onValueChange={setTab}
          variant="pill"
          items={[
            {
              value: "recents",
              label: `Recents${recents.length ? ` (${recents.length})` : ""}`,
              icon: <ClockIcon />,
            },
            {
              value: "files",
              label: `Files${files.length ? ` (${files.length})` : ""}`,
              icon: <FileIcon />,
            },
            {
              value: "examples",
              label: `Examples${examples.length ? ` (${examples.length})` : ""}`,
              icon: <RocketIcon />,
            },
          ]}
          panels={{
            recents:
              filteredRecents.length === 0 ? (
                <Empty>{recents.length === 0 ? "Open a file to see it here." : "No matches."}</Empty>
              ) : (
                <List>
                  {filteredRecents.map((path) => (
                    <Row
                      key={path}
                      label={path}
                      onPick={() => pick("file", path)}
                      trailing={
                        <button
                          type="button"
                          aria-label={`Remove ${path} from recents`}
                          className="text-fg-subtle hover:text-danger"
                          onClick={(e) => {
                            e.stopPropagation();
                            removeRecent(path);
                          }}
                        >
                          <TrashIcon />
                        </button>
                      }
                    />
                  ))}
                  {recents.length > 0 && (
                    <li className="pt-2 mt-2 border-t border-border-default">
                      <button
                        type="button"
                        className="text-xs text-fg-subtle hover:text-danger"
                        onClick={clearRecents}
                      >
                        Clear all recents
                      </button>
                    </li>
                  )}
                </List>
              ),
            files:
              filteredFiles.length === 0 ? (
                <Empty>{files.length === 0 ? "No files in workspace." : "No matches."}</Empty>
              ) : (
                <List>
                  {filteredFiles.map((f) => (
                    <Row key={f.name} label={f.name} onPick={() => pick("file", f.name)} />
                  ))}
                </List>
              ),
            examples:
              filteredExamples.length === 0 ? (
                <Empty>{examples.length === 0 ? "No examples available." : "No matches."}</Empty>
              ) : (
                <List>
                  {filteredExamples.map((n) => (
                    <Row key={n} label={n} onPick={() => pick("example", n)} />
                  ))}
                </List>
              ),
          }}
          listClassName="mb-2"
        />
      </div>
    </Dialog>
  );
}

function Empty({ children }: { children: React.ReactNode }) {
  return <p className="text-xs text-fg-subtle py-6 text-center">{children}</p>;
}

function List({ children }: { children: React.ReactNode }) {
  return (
    <ul className="max-h-[320px] overflow-y-auto divide-y divide-border-default">{children}</ul>
  );
}

function Row({
  label,
  onPick,
  trailing,
}: {
  label: string;
  onPick: () => void;
  trailing?: React.ReactNode;
}) {
  // The trailing slot can itself be a <button> (e.g. "remove from
  // recents"), so it must be a DOM sibling of the row's main button —
  // not a child — to avoid the "<button> cannot be a descendant of
  // <button>" HTML invalidity. Absolute positioning preserves the
  // visual layout while keeping the elements as siblings.
  return (
    <li className="relative">
      <button
        type="button"
        onClick={onPick}
        className="w-full flex items-center px-2 py-2 text-sm text-fg-default hover:bg-surface-2 rounded-sm"
      >
        <span className="truncate text-left flex-1">{label}</span>
        {trailing && <span aria-hidden className="w-5 shrink-0" />}
      </button>
      {trailing && (
        <span className="absolute right-2 top-1/2 -translate-y-1/2">
          {trailing}
        </span>
      )}
    </li>
  );
}
