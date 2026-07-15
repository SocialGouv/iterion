import { forwardRef, type SelectHTMLAttributes } from "react";
import { ChevronDown } from "lucide-react";

export interface SelectProps extends Omit<SelectHTMLAttributes<HTMLSelectElement>, "size"> {
  error?: boolean;
  size?: "sm" | "md";
  /** Shrink to content instead of filling the row — for inline / toolbar use. */
  fit?: boolean;
}

const sizeClass = {
  sm: "h-7 text-xs px-2 pr-7",
  md: "h-9 text-sm px-2.5 pr-8",
};

export const Select = forwardRef<HTMLSelectElement, SelectProps>(function Select(
  { className = "", error = false, size = "sm", fit = false, children, ...rest },
  ref,
) {
  const ringClass = error
    ? "border-danger focus:border-danger focus:ring-1 focus:ring-danger"
    : "border-border-strong focus:border-accent focus:ring-1 focus:ring-accent";
  const base =
    "bg-surface-1 text-fg-default rounded-md border outline-none transition-colors appearance-none disabled:opacity-60 disabled:cursor-not-allowed";
  const widthClass = fit ? "" : "w-full";
  // Chevron: an overlaid Lucide icon coloured via the token so it
  // follows the theme. A `fill='currentColor'` SVG baked into a CSS
  // background-image would NOT inherit the host colour — data-URI
  // images resolve currentColor against their own root (i.e. black).
  return (
    <div className={`relative ${fit ? "inline-block" : "w-full"}`}>
      <select
        ref={ref}
        className={`${base} ${widthClass} ${sizeClass[size]} ${ringClass} ${className}`.trim()}
        aria-invalid={error || undefined}
        {...rest}
      >
        {children}
      </select>
      <ChevronDown
        aria-hidden
        className="pointer-events-none absolute right-2 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-fg-subtle"
      />
    </div>
  );
});
