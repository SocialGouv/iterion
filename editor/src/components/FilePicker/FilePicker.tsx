import { useEffect, useMemo, useState } from "react";
import * as api from "@/api/client";
import type { FileEntry } from "@/api/types";
import { useRecentsStore } from "@/store/recents";
import { Badge, Dialog, Tabs, Input } from "@/components/ui";
import { MagnifyingGlassIcon, FileIcon, ClockIcon, RocketIcon, TrashIcon } from "@radix-ui/react-icons";
import { buildSearchResults, type SearchResult } from "./searchResults";

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

  const isSearching = query.trim() !== "";

  // Per-tab panels are only rendered when !isSearching, so the raw
  // arrays double as the displayed lists — no per-tab filtering needed.
  const searchResults = useMemo<SearchResult[]>(
    () => (isSearching ? buildSearchResults(query, recents, files, examples) : []),
    [isSearching, query, recents, files, examples],
  );

  const pick = (kind: "file" | "example", path: string) => {
    onPick(kind, path);
    onOpenChange(false);
  };

  // While searching we visually highlight the Files tab (the
  // "exhaustive" view) without mutating `tab` — clearing the query
  // restores the user's prior selection.
  const displayTab = isSearching ? "files" : tab;

  // Clicking a tab while searching clears the query so the chosen
  // tab actually becomes visible — otherwise Radix would flip
  // `displayTab` back to "files" via `isSearching`.
  const handleTabChange = (next: string) => {
    if (isSearching) setQuery("");
    setTab(next);
  };

  const renderRecentRow = (path: string, source?: RowSource) => (
    <Row
      key={source ? `recent:${path}` : path}
      label={path}
      source={source}
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
  );

  // All three tabs render the same unified list while searching, so
  // whichever tab Radix has active still shows the cross-source hits.
  const searchPanel =
    searchResults.length === 0 ? (
      <Empty>No matches.</Empty>
    ) : (
      <List>
        {searchResults.map((r) => {
          if (r.kind === "recent") return renderRecentRow(r.path, "recent");
          if (r.kind === "file") {
            return (
              <Row
                key={`file:${r.path}`}
                label={r.path}
                source="file"
                onPick={() => pick("file", r.path)}
              />
            );
          }
          return (
            <Row
              key={`example:${r.name}`}
              label={r.name}
              source="example"
              onPick={() => pick("example", r.name)}
            />
          );
        })}
      </List>
    );

  const recentsPanel =
    recents.length === 0 ? (
      <Empty>Open a file to see it here.</Empty>
    ) : (
      <List>
        {recents.map((path) => renderRecentRow(path))}
        <li className="pt-2 mt-2 border-t border-border-default">
          <button
            type="button"
            className="text-xs text-fg-subtle hover:text-danger"
            onClick={clearRecents}
          >
            Clear all recents
          </button>
        </li>
      </List>
    );

  const filesPanel =
    files.length === 0 ? (
      <Empty>No files in workspace.</Empty>
    ) : (
      <List>
        {files.map((f) => (
          <Row key={f.name} label={f.name} onPick={() => pick("file", f.name)} />
        ))}
      </List>
    );

  const examplesPanel =
    examples.length === 0 ? (
      <Empty>No examples available.</Empty>
    ) : (
      <List>
        {examples.map((n) => (
          <Row key={n} label={n} onPick={() => pick("example", n)} />
        ))}
      </List>
    );

  const panels: Record<string, React.ReactNode> = isSearching
    ? { recents: searchPanel, files: searchPanel, examples: searchPanel }
    : { recents: recentsPanel, files: filesPanel, examples: examplesPanel };

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
        {isSearching && (
          <p
            data-testid="filepicker-search-note"
            className="text-xs text-fg-subtle px-1"
            role="status"
            aria-live="polite"
          >
            Searching across all workflows ({searchResults.length}{" "}
            {searchResults.length === 1 ? "result" : "results"})
          </p>
        )}
        <Tabs
          value={displayTab}
          onValueChange={handleTabChange}
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
          panels={panels}
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

type RowSource = "recent" | "file" | "example";

const SOURCE_META: Record<RowSource, { icon: React.ReactNode; label: string }> = {
  recent: { icon: <ClockIcon />, label: "Recent" },
  file: { icon: <FileIcon />, label: "File" },
  example: { icon: <RocketIcon />, label: "Example" },
};

function SourceChip({ source }: { source: RowSource }) {
  const { icon, label } = SOURCE_META[source];
  return (
    <Badge
      variant="neutral"
      size="sm"
      leadingIcon={icon}
      className="uppercase tracking-wide mr-2 shrink-0"
      aria-label={`Source: ${label}`}
    >
      {label}
    </Badge>
  );
}

function Row({
  label,
  onPick,
  trailing,
  source,
}: {
  label: string;
  onPick: () => void;
  trailing?: React.ReactNode;
  source?: RowSource;
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
        {source && <SourceChip source={source} />}
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
