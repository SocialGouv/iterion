import type { HTMLAttributes, ReactNode } from "react";

export type BadgeVariant =
  | "neutral"
  | "info"
  | "warning"
  | "danger"
  | "success"
  | "accent";
export type BadgeSize = "sm" | "md";

export interface BadgeProps extends HTMLAttributes<HTMLSpanElement> {
  variant?: BadgeVariant;
  size?: BadgeSize;
  leadingIcon?: ReactNode;
}

const variantClass: Record<BadgeVariant, string> = {
  neutral: "bg-surface-2 text-fg-muted border-border-default",
  info: "bg-info-soft text-info-fg border-info/40",
  warning: "bg-warning-soft text-warning-fg border-warning/40",
  danger: "bg-danger-soft text-danger-fg border-danger/40",
  success: "bg-success-soft text-success-fg border-success/40",
  accent: "bg-accent-soft text-fg-default border-accent/40",
};

const sizeClass: Record<BadgeSize, string> = {
  sm: "h-4 px-1.5 text-[10px] gap-0.5",
  md: "h-5 px-2 text-xs gap-1",
};

export function Badge({
  variant = "neutral",
  size = "sm",
  leadingIcon,
  className = "",
  children,
  ...rest
}: BadgeProps) {
  const base =
    "inline-flex items-center justify-center rounded-full border font-medium leading-none whitespace-nowrap";
  return (
    <span
      className={`${base} ${variantClass[variant]} ${sizeClass[size]} ${className}`.trim()}
      {...rest}
    >
      {leadingIcon}
      {children}
    </span>
  );
}
