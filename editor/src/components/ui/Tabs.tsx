import * as RT from "@radix-ui/react-tabs";
import type { ReactNode } from "react";

export interface TabItem {
  value: string;
  label: ReactNode;
  icon?: ReactNode;
  disabled?: boolean;
}

export interface TabsProps {
  value: string;
  onValueChange: (value: string) => void;
  items: TabItem[];
  /** Render content panels keyed by tab value. */
  panels?: Record<string, ReactNode>;
  className?: string;
  listClassName?: string;
  variant?: "underline" | "pill";
}

export function Tabs({
  value,
  onValueChange,
  items,
  panels,
  className = "",
  listClassName = "",
  variant = "underline",
}: TabsProps) {
  return (
    <RT.Root
      value={value}
      onValueChange={onValueChange}
      className={`flex flex-col min-h-0 ${className}`.trim()}
    >
      <RT.List
        className={`flex items-stretch ${
          variant === "underline" ? "border-b border-border-default" : "gap-1 p-1"
        } ${listClassName}`.trim()}
      >
        {items.map((item) => (
          <RT.Trigger
            key={item.value}
            value={item.value}
            disabled={item.disabled}
            className={
              variant === "underline"
                ? "relative px-3 py-2 text-xs font-medium text-fg-subtle hover:text-fg-default data-[state=active]:text-fg-default data-[state=active]:after:content-[''] data-[state=active]:after:absolute data-[state=active]:after:left-2 data-[state=active]:after:right-2 data-[state=active]:after:bottom-0 data-[state=active]:after:h-0.5 data-[state=active]:after:bg-accent disabled:opacity-40"
                : "rounded-md px-2.5 py-1 text-xs font-medium text-fg-muted hover:bg-surface-2 hover:text-fg-default data-[state=active]:bg-surface-2 data-[state=active]:text-fg-default disabled:opacity-40"
            }
          >
            <span className="inline-flex items-center gap-1.5">
              {item.icon}
              {item.label}
            </span>
          </RT.Trigger>
        ))}
      </RT.List>
      {panels &&
        items.map((item) => (
          <RT.Content
            key={item.value}
            value={item.value}
            className="min-h-0 flex-1 outline-none"
          >
            {panels[item.value]}
          </RT.Content>
        ))}
    </RT.Root>
  );
}

export const TabsRoot = RT.Root;
export const TabsList = RT.List;
export const TabsTrigger = RT.Trigger;
export const TabsContent = RT.Content;
