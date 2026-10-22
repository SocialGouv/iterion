import { forwardRef, useId, type InputHTMLAttributes, type ReactNode } from "react";
import { HelpHint } from "./HelpHint";

export interface CheckboxProps
  extends Omit<InputHTMLAttributes<HTMLInputElement>, "type" | "size"> {
  /** Optional label rendered to the right; clicking it toggles the box. */
  label?: ReactNode;
  /** Optional `?` help affordance shown after the label. */
  help?: string;
}

const BOX =
  "h-3.5 w-3.5 shrink-0 rounded bg-surface-1 accent-accent disabled:opacity-60 disabled:cursor-not-allowed";

/**
 * Native checkbox styled with design tokens. Stays a real `<input
 * type="checkbox">` so keyboard + screen-reader semantics come for free;
 * the brand accent colours the check via `accent-accent`. Focus styling is
 * the global `:focus-visible` outline (app.css) — no per-control ring.
 *
 * `className` always lands on the outermost element (the `<label>` when a
 * label is given, otherwise the `<input>`).
 */
export const Checkbox = forwardRef<HTMLInputElement, CheckboxProps>(function Checkbox(
  { label, help, className = "", id, ...rest },
  ref,
) {
  const autoId = useId();
  const inputId = id ?? autoId;
  if (!label) {
    return (
      <input
        ref={ref}
        id={inputId}
        type="checkbox"
        className={`${BOX} ${className}`.trim()}
        {...rest}
      />
    );
  }
  return (
    <label
      htmlFor={inputId}
      className={`inline-flex items-center gap-2 text-xs text-fg-muted cursor-pointer select-none ${className}`.trim()}
    >
      <input ref={ref} id={inputId} type="checkbox" className={BOX} {...rest} />
      <span>
        {label}
        {help && <HelpHint text={help} />}
      </span>
    </label>
  );
});
