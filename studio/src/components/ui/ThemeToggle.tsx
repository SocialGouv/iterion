import { SunIcon, MoonIcon, DesktopIcon } from "@radix-ui/react-icons";

import { useThemeStore, type ThemeMode } from "@/store/theme";

const OPTIONS: { mode: ThemeMode; label: string; Icon: typeof SunIcon }[] = [
  { mode: "system", label: "System theme", Icon: DesktopIcon },
  { mode: "light", label: "Light theme", Icon: SunIcon },
  { mode: "dark", label: "Dark theme", Icon: MoonIcon },
];

export interface ThemeToggleProps {
  className?: string;
}

/**
 * ThemeToggle is a compact segmented control (System / Light / Dark) backed
 * by the global theme store. Used where there's no Settings panel in reach —
 * notably the cloud landing + public marketplace, which render outside the
 * authenticated AppShell. "System" (the default) follows the OS preference.
 */
export function ThemeToggle({ className = "" }: ThemeToggleProps) {
  const mode = useThemeStore((s) => s.mode);
  const setMode = useThemeStore((s) => s.setMode);
  return (
    <div
      role="radiogroup"
      aria-label="Theme"
      className={`inline-flex items-center gap-0.5 rounded-full border border-border-subtle bg-surface-1/70 p-0.5 backdrop-blur ${className}`.trim()}
    >
      {OPTIONS.map(({ mode: m, label, Icon }) => {
        const active = mode === m;
        return (
          <button
            key={m}
            type="button"
            role="radio"
            aria-checked={active}
            aria-label={label}
            title={label}
            onClick={() => setMode(m)}
            className={`flex h-7 w-7 items-center justify-center rounded-full transition-colors focus:outline-none focus-visible:ring-1 focus-visible:ring-accent ${
              active
                ? "bg-accent text-accent-fg"
                : "text-fg-muted hover:bg-surface-2 hover:text-fg-default"
            }`}
          >
            <Icon className="h-3.5 w-3.5" />
          </button>
        );
      })}
    </div>
  );
}
