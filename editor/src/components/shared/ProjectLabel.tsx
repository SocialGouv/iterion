import { useProjectInfo } from "@/hooks/useProjectInfo";

interface Props {
  // Visual variant: "toolbar" (next to BackendStatusPill) uses the
  // pill style; "header" (RunHeader) is slightly larger to match the
  // header typography.
  variant?: "toolbar" | "header";
}

/**
 * ProjectLabel renders the currently-selected folder/project name so
 * the user always knows which workspace they're editing. Sourced via
 * useProjectInfo (desktop currentProject → server work_dir). Renders
 * nothing when no project is resolvable (cloud mode).
 *
 * In desktop mode the label is clickable and dispatches a global
 * `iterion:open-project-switcher` CustomEvent that AuthedApp listens
 * for to open the ProjectSwitcher dialog.
 */
export default function ProjectLabel({ variant = "toolbar" }: Props) {
  const { name, dir, source } = useProjectInfo();
  if (!name) return null;

  const clickable = source === "desktop";
  const tooltip = dir
    ? `${name}\n${dir}${clickable ? "\n\nClick to switch project" : ""}`
    : name;

  const baseClasses =
    variant === "toolbar"
      ? "inline-flex items-center gap-1 px-2 py-0.5 rounded text-[10px] border border-border-subtle text-fg-muted bg-surface-2/60 max-w-[180px]"
      : "inline-flex items-center gap-1 px-2 py-0.5 rounded text-[11px] border border-border-subtle text-fg-muted bg-surface-2/60 max-w-[220px]";

  const interactive = clickable
    ? " hover:bg-surface-3 hover:text-fg-default cursor-pointer"
    : " cursor-default";

  const content = (
    <>
      <span aria-hidden className="text-fg-subtle">▸</span>
      <span className="truncate">{name}</span>
    </>
  );

  if (clickable) {
    return (
      <button
        type="button"
        className={baseClasses + interactive}
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
    <span className={baseClasses + interactive} title={tooltip}>
      {content}
    </span>
  );
}
