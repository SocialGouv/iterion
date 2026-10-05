import * as RD from "@radix-ui/react-dropdown-menu";
import type { ComponentPropsWithoutRef, ReactNode } from "react";
import { ChevronRightIcon } from "@radix-ui/react-icons";
import { useHiddenSubtree } from "./hiddenSubtree";

// Thin wrapper over Radix DropdownMenu, styled to match the rest of the
// UI kit (same surface / border / shadow tokens as Popover.tsx). Unlike
// Popover it supports nested submenus (Sub / SubTrigger / SubContent),
// which the Toolbar's File menu uses for "Open recent" and "Examples".
//
// Items, separators and labels share one row style so the menu reads as
// a single coherent list whether the entry is a leaf, a submenu trigger,
// or a section header.

const ROW_CLASS =
  "w-full flex items-center gap-2 rounded px-2 py-1.5 text-left text-xs text-fg-default outline-none cursor-default select-none data-[highlighted]:bg-surface-2 data-[disabled]:opacity-50 data-[disabled]:pointer-events-none";

const CONTENT_CLASS =
  "z-[var(--z-popover)] min-w-[220px] rounded-md border border-border-default bg-surface-1 p-1 text-fg-default shadow-[var(--shadow-popover)] animate-fade-in";

export interface DropdownMenuProps {
  trigger: ReactNode;
  children: ReactNode;
  side?: "top" | "right" | "bottom" | "left";
  align?: "start" | "center" | "end";
  open?: boolean;
  onOpenChange?: (open: boolean) => void;
  contentClassName?: string;
}

export function DropdownMenu({
  trigger,
  children,
  side = "bottom",
  align = "start",
  open,
  onOpenChange,
  contentClassName = "",
}: DropdownMenuProps) {
  const hidden = useHiddenSubtree();
  return (
    <RD.Root open={open} onOpenChange={onOpenChange}>
      <RD.Trigger asChild>{trigger}</RD.Trigger>
      {!hidden && (
      <RD.Portal>
        <RD.Content
          side={side}
          align={align}
          sideOffset={6}
          className={`${CONTENT_CLASS} ${contentClassName}`.trim()}
        >
          {children}
        </RD.Content>
      </RD.Portal>
      )}
    </RD.Root>
  );
}

export interface DropdownMenuItemProps
  extends Omit<ComponentPropsWithoutRef<typeof RD.Item>, "asChild"> {
  icon?: ReactNode;
  shortcut?: string;
}

export function DropdownMenuItem({
  icon,
  shortcut,
  children,
  className = "",
  ...rest
}: DropdownMenuItemProps) {
  return (
    <RD.Item {...rest} className={`${ROW_CLASS} ${className}`.trim()}>
      {icon !== undefined && <span className="text-fg-muted shrink-0">{icon}</span>}
      <span className="flex-1 truncate">{children}</span>
      {shortcut && (
        <span className="text-caption text-fg-subtle font-mono shrink-0">{shortcut}</span>
      )}
    </RD.Item>
  );
}

// A submenu whose trigger renders inline (with a trailing chevron) and
// whose content flies out to the side, holding `children` as its items.
export interface DropdownMenuSubProps {
  icon?: ReactNode;
  label: ReactNode;
  children: ReactNode;
  disabled?: boolean;
  contentClassName?: string;
}

export function DropdownMenuSub({
  icon,
  label,
  children,
  disabled,
  contentClassName = "",
}: DropdownMenuSubProps) {
  const hidden = useHiddenSubtree();
  return (
    <RD.Sub>
      <RD.SubTrigger disabled={disabled} className={ROW_CLASS}>
        {icon !== undefined && <span className="text-fg-muted shrink-0">{icon}</span>}
        <span className="flex-1 truncate">{label}</span>
        <ChevronRightIcon className="text-fg-subtle shrink-0" />
      </RD.SubTrigger>
      {!hidden && (
      <RD.Portal>
        <RD.SubContent
          sideOffset={2}
          alignOffset={-4}
          className={`${CONTENT_CLASS} ${contentClassName}`.trim()}
        >
          {children}
        </RD.SubContent>
      </RD.Portal>
      )}
    </RD.Sub>
  );
}

export function DropdownMenuSeparator() {
  return <RD.Separator className="my-1 h-px bg-border-default" />;
}

export function DropdownMenuLabel({ children }: { children: ReactNode }) {
  return (
    <RD.Label className="px-2 py-1 text-caption text-fg-subtle">{children}</RD.Label>
  );
}
