import { forwardRef, useId, type InputHTMLAttributes, type ReactNode } from "react";

export interface RadioProps
  extends Omit<InputHTMLAttributes<HTMLInputElement>, "type" | "size"> {
  /** Optional label rendered to the right; clicking it selects the radio. */
  label?: ReactNode;
}

const DOT =
  "h-3.5 w-3.5 shrink-0 bg-surface-1 accent-accent disabled:opacity-60 disabled:cursor-not-allowed";

/**
 * Native radio styled with design tokens. Real `<input type="radio">` so
 * arrow-key roving + screen-reader semantics come for free; the brand accent
 * colours the dot via `accent-accent`. Prefer {@link RadioGroup} for the
 * common labelled-set case. Focus styling is the global `:focus-visible`.
 *
 * `className` always lands on the outermost element (the `<label>` when a
 * label is given, otherwise the `<input>`).
 */
export const Radio = forwardRef<HTMLInputElement, RadioProps>(function Radio(
  { label, className = "", id, ...rest },
  ref,
) {
  const autoId = useId();
  const inputId = id ?? autoId;
  if (!label) {
    return (
      <input
        ref={ref}
        id={inputId}
        type="radio"
        className={`${DOT} ${className}`.trim()}
        {...rest}
      />
    );
  }
  return (
    <label
      htmlFor={inputId}
      className={`inline-flex items-center gap-2 text-xs text-fg-muted cursor-pointer select-none ${className}`.trim()}
    >
      <input ref={ref} id={inputId} type="radio" className={DOT} {...rest} />
      <span>{label}</span>
    </label>
  );
});
