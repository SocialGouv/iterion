import { forwardRef, type InputHTMLAttributes, type ReactNode } from "react";

export interface InputProps extends Omit<InputHTMLAttributes<HTMLInputElement>, "size"> {
  error?: boolean;
  leadingIcon?: ReactNode;
  trailingIcon?: ReactNode;
  /** Compact size matching the existing FormField inputs. */
  size?: "sm" | "md";
}

const sizeClass = {
  sm: "h-7 text-xs px-2",
  md: "h-9 text-sm px-2.5",
};

export const Input = forwardRef<HTMLInputElement, InputProps>(function Input(
  {
    className = "",
    error = false,
    leadingIcon,
    trailingIcon,
    size = "sm",
    ...rest
  },
  ref,
) {
  const ringClass = error
    ? "border-danger focus:border-danger focus:ring-1 focus:ring-danger"
    : "border-border-strong focus:border-accent focus:ring-1 focus:ring-accent";
  const base =
    "w-full bg-surface-1 text-fg-default rounded-md border outline-none transition-colors placeholder:text-fg-subtle disabled:opacity-60 disabled:cursor-not-allowed";

  if (leadingIcon || trailingIcon) {
    return (
      <div className="relative w-full">
        {leadingIcon && (
          <span className="absolute left-2 top-1/2 -translate-y-1/2 text-fg-subtle pointer-events-none">
            {leadingIcon}
          </span>
        )}
        <input
          ref={ref}
          className={`${base} ${sizeClass[size]} ${ringClass} ${
            leadingIcon ? "pl-7" : ""
          } ${trailingIcon ? "pr-7" : ""} ${className}`.trim()}
          {...rest}
        />
        {trailingIcon && (
          <span className="absolute right-2 top-1/2 -translate-y-1/2 text-fg-subtle pointer-events-none">
            {trailingIcon}
          </span>
        )}
      </div>
    );
  }

  return (
    <input
      ref={ref}
      className={`${base} ${sizeClass[size]} ${ringClass} ${className}`.trim()}
      {...rest}
    />
  );
});
