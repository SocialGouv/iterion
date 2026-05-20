import { useEffect, useState } from "react";

import { useDocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";
import { createEmptyDocument } from "@/lib/defaults";
import { Button, Dialog } from "@/components/ui";
import { listExamples, loadExample } from "@/api/client";
import {
  FileIcon,
  RocketIcon,
  ArrowLeftIcon,
  StackIcon,
} from "@radix-ui/react-icons";

/**
 * Rendered over the canvas when the active workflow has zero edges and no
 * declared agents/judges/routers/humans/tools — i.e. the user is staring at
 * a blank slate. Offers three obvious next actions.
 */
export default function CanvasEmpty() {
  const setDocument = useDocumentStore((s) => s.setDocument);
  const setDiagnostics = useDocumentStore((s) => s.setDiagnostics);
  const setCurrentFilePath = useDocumentStore((s) => s.setCurrentFilePath);
  const setCurrentSource = useDocumentStore((s) => s.setCurrentSource);
  const markSaved = useDocumentStore((s) => s.markSaved);
  const toggleLibraryPanel = useUIStore((s) => s.toggleLibraryPanel);
  const libraryExpanded = useUIStore((s) => s.libraryExpanded);
  const setFilePickerOpen = useUIStore((s) => s.setFilePickerOpen);
  const [examplesOpen, setExamplesOpen] = useState(false);

  const handleStartBlank = () => {
    setDocument(createEmptyDocument());
    setDiagnostics([], []);
    setCurrentFilePath(null);
    markSaved();
  };

  const handleLoadExample = async (name: string) => {
    try {
      const res = await loadExample(name);
      setDocument(res.document);
      setCurrentSource(res.source);
      setCurrentFilePath(null);
      setDiagnostics([], []);
      markSaved();
      setExamplesOpen(false);
    } catch {
      // The server returns useful errors; the modal stays open so the
      // user can try another example.
    }
  };

  return (
    <div className="absolute inset-0 z-10 flex items-center justify-center pointer-events-none">
      <div className="pointer-events-auto rounded-lg border border-border-default bg-surface-1/95 backdrop-blur px-6 py-5 text-center shadow-[var(--shadow-popover)] max-w-lg">
        <h2 className="text-base font-semibold text-fg-default mb-1">No workflow loaded</h2>
        <p className="text-xs text-fg-subtle mb-4">
          Start blank, pick a starter example, or drop a node from the library on the left.
        </p>
        <div className="grid grid-cols-4 gap-2">
          <Tile
            icon={<FileIcon />}
            label="Start blank"
            description="Empty workflow"
            onClick={handleStartBlank}
          />
          <Tile
            icon={<StackIcon />}
            label="Examples"
            description="Browse starters"
            onClick={() => setExamplesOpen(true)}
          />
          <Tile
            icon={<RocketIcon />}
            label="Open file"
            description="Recents · Files"
            onClick={() => setFilePickerOpen(true)}
          />
          <Tile
            icon={<ArrowLeftIcon />}
            label="Drag a node"
            description="Library →"
            onClick={() => {
              if (!libraryExpanded) toggleLibraryPanel();
            }}
          />
        </div>
      </div>
      {examplesOpen && (
        <ExamplesPicker
          onClose={() => setExamplesOpen(false)}
          onPick={(name) => void handleLoadExample(name)}
        />
      )}
    </div>
  );
}

// ExamplesPicker fetches the list once on open and renders it as a
// scrollable card grid. The names returned by the backend are repo-
// relative paths (e.g. "feature_dev/main.bot") — we split on the
// last slash so the card header carries the bundle name and the
// sub-line carries the directory.
function ExamplesPicker({
  onClose,
  onPick,
}: {
  onClose: () => void;
  onPick: (name: string) => void;
}) {
  const [examples, setExamples] = useState<string[] | null>(null);
  const [err, setErr] = useState<string | null>(null);
  useEffect(() => {
    let cancelled = false;
    listExamples()
      .then((list) => {
        if (!cancelled) setExamples(list);
      })
      .catch((e) => {
        if (!cancelled) setErr((e as Error).message);
      });
    return () => {
      cancelled = true;
    };
  }, []);
  return (
    <Dialog open={true} onOpenChange={onClose} title="Choose a starter example">
      <div className="space-y-2 max-h-[60vh] overflow-auto pointer-events-auto">
        {err && (
          <div className="text-xs text-danger-fg">
            Could not load examples: {err}
          </div>
        )}
        {!err && examples === null && (
          <div className="text-xs text-fg-muted">Loading examples…</div>
        )}
        {examples?.length === 0 && (
          <div className="text-xs text-fg-muted">
            No examples available. The server didn't embed any.
          </div>
        )}
        {examples?.map((name) => {
          const slash = name.lastIndexOf("/");
          const label = slash >= 0 ? name.slice(slash + 1) : name;
          const subline = slash >= 0 ? name.slice(0, slash) : "examples";
          return (
            <button
              key={name}
              type="button"
              onClick={() => onPick(name)}
              className="w-full text-left rounded border border-border-default px-3 py-2 hover:bg-surface-2 transition-colors"
            >
              <div className="font-medium text-sm text-fg-default">{label}</div>
              <div className="text-[10px] text-fg-subtle font-mono">{subline}</div>
            </button>
          );
        })}
      </div>
    </Dialog>
  );
}

function Tile({
  icon,
  label,
  description,
  onClick,
}: {
  icon: React.ReactNode;
  label: string;
  description: string;
  onClick: () => void;
}) {
  return (
    <Button
      variant="secondary"
      size="md"
      className="!h-auto flex-col items-start gap-1 py-3 px-3 text-left whitespace-normal"
      onClick={onClick}
    >
      <span className="inline-flex items-center gap-1.5 text-fg-default">
        {icon}
        <span className="font-semibold text-xs">{label}</span>
      </span>
      <span className="text-[10px] text-fg-subtle font-normal">{description}</span>
    </Button>
  );
}
