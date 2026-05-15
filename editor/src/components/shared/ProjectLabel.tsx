import { FolderOpen } from "lucide-react";

import { useProjectInfo } from "@/hooks/useProjectInfo";

// ProjectLabel renders the currently-selected folder/project name so
// the user always knows which workspace they're editing. Sourced via
// useProjectInfo (desktop currentProject → server work_dir). Renders
// nothing when no project is resolvable (cloud mode). Clickable in
// desktop and local-server modes: dispatches a global
// `iterion:open-project-switcher` event handled in App.tsx.
export default function ProjectLabel() {
  const { name, dir, source } = useProjectInfo();
  if (!name) return null;

  // "server" comes from /api/server/info, set only in local mode.
  const clickable = source === "desktop" || source === "server";
  const tooltip = dir
    ? `Project: ${name}\n${dir}${clickable ? "\n\nClick to switch project" : ""}`
    : name;

  const base =
    "inline-flex items-center gap-1.5 px-2.5 py-1 rounded-md font-medium text-xs" +
    " border border-accent/40 bg-accent-soft text-accent leading-none max-w-[220px]";
  const interactive = clickable
    ? " hover:border-accent hover:bg-accent/20 cursor-pointer transition-colors"
    : " cursor-default";

  const content = (
    <>
      <FolderOpen size={13} className="shrink-0" />
      <span className="truncate">{name}</span>
    </>
  );

  if (clickable) {
    return (
      <button
        type="button"
        className={base + interactive}
        title={tooltip}
        onClick={() =>
          window.dispatchEvent(new CustomEvent("iterion:open-project-switcher"))
        }
      >
        {content}
      </button>
    );
  }
  return (
    <span className={base + interactive} title={tooltip}>
      {content}
    </span>
  );
}
