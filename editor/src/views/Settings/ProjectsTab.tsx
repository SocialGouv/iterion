import { Button } from "@/components/ui";

import { useDesktop } from "@/hooks/useDesktop";
import { desktop } from "@/lib/desktopBridge";

export default function ProjectsTab() {
  const { projects, currentProject, switchProject, removeProject } = useDesktop();

  return (
    <div className="flex flex-col gap-2 p-4">
      {projects.length === 0 && (
        <p className="text-fg-subtle text-sm">No projects yet.</p>
      )}
      <ul className="flex flex-col">
        {projects.map((p) => {
          const isCurrent = p.id === currentProject?.id;
          return (
            <li
              key={p.id}
              className="flex items-center gap-2 border-b border-border-default py-2"
            >
              <div className="min-w-0 flex-1">
                <div className="flex items-center gap-2">
                  <span className="font-semibold">{p.name}</span>
                  {isCurrent && (
                    <span className="text-xs text-success-fg">(current)</span>
                  )}
                </div>
                <div className="text-xs text-fg-subtle truncate">{p.dir}</div>
              </div>
              {!isCurrent && (
                <Button size="sm" variant="primary" onClick={() => switchProject(p.id)}>
                  Open
                </Button>
              )}
              <Button size="sm" variant="ghost" onClick={() => desktop.revealInFinder(p.dir)}>
                Reveal
              </Button>
              <Button size="sm" variant="ghost" onClick={() => removeProject(p.id)}>
                Remove
              </Button>
            </li>
          );
        })}
      </ul>
    </div>
  );
}
