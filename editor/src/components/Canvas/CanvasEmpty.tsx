import { useDocumentStore } from "@/store/document";
import { useUIStore } from "@/store/ui";
import { createEmptyDocument } from "@/lib/defaults";
import { Button } from "@/components/ui";
import { FileIcon, RocketIcon, ArrowLeftIcon } from "@radix-ui/react-icons";

/**
 * Rendered over the canvas when the active workflow has zero edges and no
 * declared agents/judges/routers/humans/tools — i.e. the user is staring at
 * a blank slate. Offers three obvious next actions.
 */
export default function CanvasEmpty() {
  const setDocument = useDocumentStore((s) => s.setDocument);
  const setDiagnostics = useDocumentStore((s) => s.setDiagnostics);
  const setCurrentFilePath = useDocumentStore((s) => s.setCurrentFilePath);
  const markSaved = useDocumentStore((s) => s.markSaved);
  const toggleLibraryPanel = useUIStore((s) => s.toggleLibraryPanel);
  const libraryExpanded = useUIStore((s) => s.libraryExpanded);
  const setFilePickerOpen = useUIStore((s) => s.setFilePickerOpen);

  const handleStartBlank = () => {
    setDocument(createEmptyDocument());
    setDiagnostics([], []);
    setCurrentFilePath(null);
    markSaved();
  };

  return (
    <div className="absolute inset-0 z-10 flex items-center justify-center pointer-events-none">
      <div className="pointer-events-auto rounded-lg border border-border-default bg-surface-1/95 backdrop-blur px-6 py-5 text-center shadow-xl max-w-md">
        <h2 className="text-base font-semibold text-fg-default mb-1">No workflow loaded</h2>
        <p className="text-xs text-fg-subtle mb-4">
          Start blank, open an example, or drop a node from the library on the left.
        </p>
        <div className="grid grid-cols-3 gap-2">
          <Tile
            icon={<FileIcon />}
            label="Start blank"
            description="Empty workflow"
            onClick={handleStartBlank}
          />
          <Tile
            icon={<RocketIcon />}
            label="Open file"
            description="Recents · Files · Examples"
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
    </div>
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
