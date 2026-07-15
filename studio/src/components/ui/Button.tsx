import { forwardRef, type ButtonHTMLAttributes, type ReactNode } from "react";
import { Spinner } from "./Spinner";

export type ButtonVariant = "primary" | "secondary" | "ghost" | "danger" | "success";
export type ButtonSize = "sm" | "md";

export interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: ButtonVariant;
  size?: ButtonSize;
  loading?: boolean;
  leadingIcon?: ReactNode;
  trailingIcon?: ReactNode;
}

const variantClass: Record<ButtonVariant, string> = {
  // Disabled primary keeps the accent fill dimmed (uniform opacity)
  // instead of collapsing to a gray surface chip — a gated submit must
  // still read as "the primary button, currently unavailable", not as
  // a passive label. disabled:hover:bg-accent pins the fill because a
  // disabled button still matches :hover in CSS.
  primary:
    "bg-accent text-fg-onAccent hover:bg-accent-hover disabled:opacity-50 disabled:hover:bg-accent",
  secondary:
    "bg-surface-2 text-fg-default hover:bg-surface-3 disabled:bg-surface-1 disabled:text-fg-subtle border border-border-default",
  ghost:
    "bg-transparent text-fg-muted hover:bg-surface-2 hover:text-fg-default disabled:text-fg-subtle",
  danger:
    "bg-danger text-fg-onAccent hover:opacity-90 disabled:bg-surface-2 disabled:text-fg-subtle",
  success:
    "bg-success text-fg-onAccent hover:opacity-90 disabled:bg-surface-2 disabled:text-fg-subtle",
};

const sizeClass: Record<ButtonSize, string> = {
  sm: "h-7 px-2.5 text-xs gap-1.5",
  md: "h-9 px-3 text-sm gap-2",
};

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(function Button(
  {
    variant = "secondary",
    size = "md",
    loading = false,
    leadingIcon,
    trailingIcon,
    className = "",
    disabled,
    children,
    type = "button",
    ...rest
  },
  ref,
) {
  const base =
    "inline-flex items-center justify-center rounded-md font-medium transition-colors disabled:cursor-not-allowed select-none";
  return (
    <button
      ref={ref}
      type={type}
      disabled={disabled || loading}
      className={`${base} ${variantClass[variant]} ${sizeClass[size]} ${className}`.trim()}
      {...rest}
    >
      {loading ? <Spinner size="sm" /> : leadingIcon}
      {children}
      {!loading && trailingIcon}
    </button>
  );
});
