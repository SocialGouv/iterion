import { forwardRef, type ButtonHTMLAttributes, type ReactNode } from "react";
import { Tooltip } from "./Tooltip";

export type IconButtonVariant = "secondary" | "ghost" | "danger" | "primary";
export type IconButtonSize = "sm" | "md";

export interface IconButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: IconButtonVariant;
  size?: IconButtonSize;
  /** Accessible label, also used as tooltip text when `tooltip` is unset. */
  label: string;
  tooltip?: ReactNode;
  active?: boolean;
}

const variantClass: Record<IconButtonVariant, string> = {
  primary: "bg-accent text-fg-onAccent hover:bg-accent-hover",
  secondary:
    "bg-surface-2 text-fg-default hover:bg-surface-3 border border-border-default",
  ghost: "bg-transparent text-fg-muted hover:bg-surface-2 hover:text-fg-default",
  danger: "bg-transparent text-danger hover:bg-danger-soft",
};

const sizeClass: Record<IconButtonSize, string> = {
  sm: "h-7 w-7 text-sm",
  md: "h-9 w-9 text-base",
};

export const IconButton = forwardRef<HTMLButtonElement, IconButtonProps>(
  function IconButton(
    {
      variant = "ghost",
      size = "md",
      label,
      tooltip,
      active = false,
      className = "",
      type = "button",
      children,
      disabled,
      ...rest
    },
    ref,
  ) {
    const activeClass = active
      ? "ring-1 ring-accent text-fg-default bg-surface-3"
      : "";
    const base =
      "inline-flex items-center justify-center rounded-md transition-colors disabled:opacity-50 disabled:cursor-not-allowed select-none";
    const button = (
      <button
        ref={ref}
        type={type}
        aria-label={label}
        aria-pressed={active || undefined}
        disabled={disabled}
        className={`${base} ${variantClass[variant]} ${sizeClass[size]} ${activeClass} ${className}`.trim()}
        {...rest}
      >
        {children}
      </button>
    );
    const content = tooltip ?? label;
    if (!content) return button;
    return <Tooltip content={content}>{button}</Tooltip>;
  },
);
