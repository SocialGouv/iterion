import { ExclamationTriangleIcon, CrossCircledIcon } from "@radix-ui/react-icons";
import type { AttributedDiagnostic } from "@/lib/diagnostics";
import { dominantSeverity } from "@/lib/diagnostics";
import { getHint } from "@/lib/diagnosticHints";
import { Popover } from "@/components/ui";

export interface DiagnosticBadgeProps {
  diagnostics: AttributedDiagnostic[];
  /** Optional callback when "Reveal" is clicked (e.g., select node/edge). */
  onReveal?: () => void;
  side?: "top" | "right" | "bottom" | "left";
  align?: "start" | "center" | "end";
  /** Visual size of the trigger badge. */
  size?: "sm" | "md";
}

const sizeClasses = {
  sm: "h-4 min-w-4 text-[10px] px-1",
  md: "h-5 min-w-5 text-xs px-1.5",
};

/**
 * Compact badge showing the count + dominant severity for a set of
 * diagnostics attributed to a single node or edge. Hovering opens a popover
 * with the full list (code, message, hint, optional reveal action).
 */
export default function DiagnosticBadge({
  diagnostics,
  onReveal,
  side = "right",
  align = "start",
  size = "sm",
}: DiagnosticBadgeProps) {
  if (diagnostics.length === 0) return null;
  const sev = dominantSeverity(diagnostics);
  if (!sev) return null;

  const colorClass =
    sev === "error"
      ? "bg-danger text-fg-onAccent border-danger"
      : "bg-warning text-fg-onAccent border-warning";
  const Icon = sev === "error" ? CrossCircledIcon : ExclamationTriangleIcon;

  return (
    <Popover
      side={side}
      align={align}
      contentClassName="max-w-[320px]"
      trigger={
        <button
          type="button"
          className={`inline-flex items-center justify-center gap-1 rounded-full border ${colorClass} ${sizeClasses[size]} font-semibold leading-none shadow-md cursor-pointer`}
          aria-label={`${diagnostics.length} ${sev}${diagnostics.length === 1 ? "" : "s"}`}
          onClick={(e) => {
            // Don't let the click propagate to underlying node/edge selection.
            e.stopPropagation();
          }}
        >
          <Icon />
          <span>{diagnostics.length}</span>
        </button>
      }
    >
      <div className="p-3 space-y-2">
        {diagnostics.map((d, i) => {
          const staticHint = d.code ? getHint(d.code) : undefined;
          const hintText = d.hint ?? staticHint?.hint;
          return (
            <div key={i} className="text-xs">
              <div className="flex items-center gap-2 mb-0.5">
                <span
                  className={`inline-flex items-center gap-1 rounded px-1.5 py-0.5 text-[10px] font-mono ${
                    d.severity === "error"
                      ? "bg-danger-soft text-danger-fg"
                      : "bg-warning-soft text-warning-fg"
                  }`}
                >
                  {d.code || (d.severity === "error" ? "ERR" : "WARN")}
                </span>
                {staticHint && <span className="font-semibold text-fg-default">{staticHint.title}</span>}
              </div>
              <p className="text-fg-muted mb-0.5">{d.message}</p>
              {hintText && <p className="text-fg-subtle italic">{hintText}</p>}
            </div>
          );
        })}
        {onReveal && (
          <div className="pt-2 border-t border-border-default">
            <button
              type="button"
              className="text-xs text-accent hover:underline"
              onClick={(e) => {
                e.stopPropagation();
                onReveal();
              }}
            >
              Reveal in editor
            </button>
          </div>
        )}
      </div>
    </Popover>
  );
}
