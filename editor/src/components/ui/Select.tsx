import { forwardRef, type SelectHTMLAttributes } from "react";

export interface SelectProps extends Omit<SelectHTMLAttributes<HTMLSelectElement>, "size"> {
  error?: boolean;
  size?: "sm" | "md";
}

const sizeClass = {
  sm: "h-7 text-xs px-2 pr-7",
  md: "h-9 text-sm px-2.5 pr-8",
};

export const Select = forwardRef<HTMLSelectElement, SelectProps>(function Select(
  { className = "", error = false, size = "sm", children, ...rest },
  ref,
) {
  const ringClass = error
    ? "border-danger focus:border-danger focus:ring-1 focus:ring-danger"
    : "border-border-strong focus:border-accent focus:ring-1 focus:ring-accent";
  const base =
    "w-full bg-surface-1 text-fg-default rounded-md border outline-none transition-colors appearance-none disabled:opacity-60 disabled:cursor-not-allowed";
  // Inline SVG chevron, theme-aware via currentColor.
  const chevron =
    "bg-[length:14px] bg-no-repeat bg-[right_0.5rem_center] bg-[image:var(--select-chevron)]";
  return (
    <div className="relative w-full">
      <select
        ref={ref}
        className={`${base} ${sizeClass[size]} ${ringClass} ${chevron} ${className}`.trim()}
        style={{
          // currentColor-aware chevron via CSS variable
          // eslint-disable-next-line @typescript-eslint/no-explicit-any
          ["--select-chevron" as any]:
            "url(\"data:image/svg+xml;utf8,<svg xmlns='http://www.w3.org/2000/svg' width='14' height='14' viewBox='0 0 20 20' fill='%239ca3af'><path d='M5.5 7l4.5 5 4.5-5z'/></svg>\")",
        }}
        {...rest}
      >
        {children}
      </select>
    </div>
  );
});
