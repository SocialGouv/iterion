import { SunIcon, MoonIcon, DesktopIcon } from "@radix-ui/react-icons";
import { useThemeStore, type ThemeMode } from "@/store/theme";
import { IconButton } from "./IconButton";

const NEXT_LABEL: Record<ThemeMode, string> = {
  system: "Switch to light",
  light: "Switch to dark",
  dark: "Switch to system",
};

export default function ThemeToggle() {
  const mode = useThemeStore((s) => s.mode);
  const cycleMode = useThemeStore((s) => s.cycleMode);

  const Icon = mode === "system" ? DesktopIcon : mode === "light" ? SunIcon : MoonIcon;

  return (
    <IconButton
      label={NEXT_LABEL[mode]}
      tooltip={`Theme: ${mode}`}
      size="sm"
      variant="ghost"
      onClick={cycleMode}
    >
      <Icon />
    </IconButton>
  );
}
