import { SunIcon, MoonIcon, DesktopIcon } from "@radix-ui/react-icons";

import { useThemeStore, type ThemeMode } from "@/store/theme";
import { useUIStore } from "@/store/ui";

const OPTIONS: { mode: ThemeMode; label: string; description: string; Icon: typeof SunIcon }[] = [
  {
    mode: "system",
    label: "System",
    description: "Match the OS appearance",
    Icon: DesktopIcon,
  },
  { mode: "light", label: "Light", description: "Always light", Icon: SunIcon },
  { mode: "dark", label: "Dark", description: "Always dark", Icon: MoonIcon },
];

export default function AppearanceTab() {
  const mode = useThemeStore((s) => s.mode);
  const setMode = useThemeStore((s) => s.setMode);
  const chatEnterSubmits = useUIStore((s) => s.chatEnterSubmits);
  const setChatEnterSubmits = useUIStore((s) => s.setChatEnterSubmits);
  return (
    <div className="flex flex-col gap-6 p-4 text-sm">
      <section className="flex flex-col gap-3">
        <div>
          <div className="font-medium text-fg-default mb-1">Theme</div>
          <p className="text-xs text-fg-subtle">
            Persists in this browser. "System" follows your OS preference.
          </p>
        </div>
        <div className="grid grid-cols-3 gap-2">
          {OPTIONS.map(({ mode: m, label, description, Icon }) => {
            const active = mode === m;
            return (
              <button
                key={m}
                type="button"
                onClick={() => setMode(m)}
                className={`flex flex-col items-start gap-1 rounded border p-3 text-left transition-colors ${
                  active
                    ? "border-accent bg-accent-soft/30 ring-1 ring-accent/40"
                    : "border-border-default bg-surface-1 hover:bg-surface-2"
                }`}
                aria-pressed={active}
              >
                <span className="flex items-center gap-2 text-fg-default font-medium">
                  <Icon className="w-4 h-4" />
                  {label}
                </span>
                <span className="text-xs text-fg-subtle">{description}</span>
              </button>
            );
          })}
        </div>
      </section>

      <section className="flex flex-col gap-3">
        <div>
          <div className="font-medium text-fg-default mb-1">Chat input</div>
          <p className="text-xs text-fg-subtle">
            How chat textareas (AgentChatbox + WhatsNext human turns)
            interpret the Enter key.
          </p>
        </div>
        <div className="grid grid-cols-2 gap-2">
          <button
            type="button"
            onClick={() => setChatEnterSubmits(true)}
            className={`flex flex-col items-start gap-1 rounded border p-3 text-left transition-colors ${
              chatEnterSubmits
                ? "border-accent bg-accent-soft/30 ring-1 ring-accent/40"
                : "border-border-default bg-surface-1 hover:bg-surface-2"
            }`}
            aria-pressed={chatEnterSubmits}
          >
            <span className="text-fg-default font-medium">Enter submits</span>
            <span className="text-xs text-fg-subtle">
              Enter sends the message. Shift+Enter inserts a newline.
            </span>
          </button>
          <button
            type="button"
            onClick={() => setChatEnterSubmits(false)}
            className={`flex flex-col items-start gap-1 rounded border p-3 text-left transition-colors ${
              !chatEnterSubmits
                ? "border-accent bg-accent-soft/30 ring-1 ring-accent/40"
                : "border-border-default bg-surface-1 hover:bg-surface-2"
            }`}
            aria-pressed={!chatEnterSubmits}
          >
            <span className="text-fg-default font-medium">
              Cmd/Ctrl+Enter submits
            </span>
            <span className="text-xs text-fg-subtle">
              Enter inserts a newline. Cmd/Ctrl+Enter sends the message
              (legacy behavior).
            </span>
          </button>
        </div>
      </section>
    </div>
  );
}
