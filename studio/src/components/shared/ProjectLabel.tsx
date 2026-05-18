import { useProjectInfo } from "@/hooks/useProjectInfo";

// Inline open-folder glyph. Radix has no folder icon and lucide carries
// the rest of its catalog along the import — this 13×13 SVG keeps the
// pill self-contained without bringing back a second icon library.
function FolderOpenGlyph() {
  return (
    <svg
      width="13"
      height="13"
      viewBox="0 0 15 15"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.2"
      strokeLinecap="round"
      strokeLinejoin="round"
      className="shrink-0"
      aria-hidden="true"
    >
      <path d="M1.5 4.5h4l1 1h7v6.5a.5.5 0 0 1-.5.5h-11a.5.5 0 0 1-.5-.5v-7.5z" />
      <path d="M2 12l1.5-4.5h11l-1.5 4.5" />
    </svg>
  );
}

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
    " border border-accent/40 bg-accent-soft text-accent leading-none max-w-[140px] sm:max-w-[220px]";
  const interactive = clickable
    ? " hover:border-accent hover:bg-accent/20 cursor-pointer transition-colors"
    : " cursor-default";

  const content = (
    <>
      <FolderOpenGlyph />
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
