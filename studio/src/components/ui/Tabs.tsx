import * as RT from "@radix-ui/react-tabs";
import type { ReactNode } from "react";

export interface TabItem<V extends string = string> {
  value: V;
  label: ReactNode;
  icon?: ReactNode;
  disabled?: boolean;
}

// Generic over the tab-value union so call sites with a closed set of
// tabs (e.g. NodeDetailPanel's TabValue) get a typed onValueChange
// instead of casting the string Radix reports. Defaults to `string`,
// so untyped call sites are unaffected.
export interface TabsProps<V extends string = string> {
  value: V;
  onValueChange: (value: V) => void;
  items: TabItem<V>[];
  /** Render content panels keyed by tab value. */
  panels?: Record<string, ReactNode>;
  className?: string;
  listClassName?: string;
  /** Extra classes applied to every trigger. Use for vertical side-nav
   *  layouts (e.g. `sm:w-full sm:text-left`) where the pill should fill the
   *  column and left-align its label instead of rendering as a centered chip. */
  triggerClassName?: string;
  variant?: "underline" | "pill";
}

export function Tabs<V extends string = string>({
  value,
  onValueChange,
  items,
  panels,
  className = "",
  listClassName = "",
  triggerClassName = "",
  variant = "underline",
}: TabsProps<V>) {
  return (
    <RT.Root
      value={value}
      // Boundary cast: Radix reports plain string, but every trigger is
      // rendered from items (value: V), so the reported value is always
      // a member of V at runtime.
      onValueChange={(v) => onValueChange(v as V)}
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
            className={`${
              variant === "underline"
                ? "relative px-3 py-2 text-xs font-medium text-fg-subtle hover:text-fg-default data-[state=active]:text-fg-default data-[state=active]:after:content-[''] data-[state=active]:after:absolute data-[state=active]:after:left-2 data-[state=active]:after:right-2 data-[state=active]:after:bottom-0 data-[state=active]:after:h-0.5 data-[state=active]:after:bg-accent disabled:opacity-40 disabled:cursor-not-allowed disabled:hover:text-fg-subtle"
                : // pill: active and hover must stay visually distinct. Hover is
                  // scoped to data-[state=inactive] so hovering the active tab
                  // never overrides its (stronger) active background — the bug
                  // that made the team/settings side-nav's current tab
                  // indistinguishable from a merely-hovered one.
                  "rounded-md px-2.5 py-1 text-xs font-medium transition-colors text-fg-muted data-[state=inactive]:hover:bg-surface-1 data-[state=inactive]:hover:text-fg-default data-[state=active]:bg-surface-2 data-[state=active]:text-fg-default data-[state=active]:font-semibold disabled:opacity-40 disabled:cursor-not-allowed disabled:hover:bg-transparent disabled:hover:text-fg-muted"
            } ${triggerClassName}`.trim()}
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
