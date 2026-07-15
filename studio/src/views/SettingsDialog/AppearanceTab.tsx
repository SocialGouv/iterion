import { useRef, type KeyboardEvent } from "react";
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

// WAI-ARIA radio-group keyboard nav: ←/↑ moves to the previous option,
// →/↓ to the next, wrapping at the edges. Selects the option as it moves
// (matches OS radio-group behaviour and the role="radio" buttons we
// render). Returns true when the handler consumed the key.
function moveRadioFocus(
  e: KeyboardEvent<HTMLButtonElement>,
  refs: (HTMLButtonElement | null)[],
  currentIndex: number,
  onSelect: (nextIndex: number) => void,
): boolean {
  const isPrev = e.key === "ArrowLeft" || e.key === "ArrowUp";
  const isNext = e.key === "ArrowRight" || e.key === "ArrowDown";
  if (!isPrev && !isNext) return false;
  e.preventDefault();
  const n = refs.length;
  const next = isPrev ? (currentIndex - 1 + n) % n : (currentIndex + 1) % n;
  onSelect(next);
  refs[next]?.focus();
  return true;
}

export default function AppearanceTab() {
  const mode = useThemeStore((s) => s.mode);
  const setMode = useThemeStore((s) => s.setMode);
  const chatEnterSubmits = useUIStore((s) => s.chatEnterSubmits);
  const setChatEnterSubmits = useUIStore((s) => s.setChatEnterSubmits);
  const themeRefs = useRef<(HTMLButtonElement | null)[]>([]);
  const chatRefs = useRef<(HTMLButtonElement | null)[]>([]);
  const currentThemeIndex = OPTIONS.findIndex((o) => o.mode === mode);
  const currentChatIndex = chatEnterSubmits ? 0 : 1;
  return (
    <div className="flex flex-col gap-6 p-4 text-sm">
      <section className="flex flex-col gap-3">
        <div>
          <div className="font-medium text-fg-default mb-1">Theme</div>
          <p className="text-xs text-fg-subtle">
            Persists in this browser. "System" follows your OS preference.
          </p>
        </div>
        <div
          className="grid grid-cols-3 gap-2"
          role="radiogroup"
          aria-label="Theme"
        >
          {OPTIONS.map(({ mode: m, label, description, Icon }, i) => {
            const active = mode === m;
            return (
              <button
                key={m}
                ref={(el) => {
                  themeRefs.current[i] = el;
                }}
                type="button"
                role="radio"
                aria-checked={active}
                tabIndex={active ? 0 : -1}
                onClick={() => setMode(m)}
                onKeyDown={(e) =>
                  moveRadioFocus(
                    e,
                    themeRefs.current,
                    currentThemeIndex < 0 ? i : currentThemeIndex,
                    (next) => setMode(OPTIONS[next]!.mode),
                  )
                }
                className={`flex flex-col items-start gap-1 rounded border p-3 text-left transition-colors focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent ${
                  active
                    ? "border-accent bg-accent-soft/30 ring-1 ring-accent/40"
                    : "border-border-default bg-surface-1 hover:bg-surface-2"
                }`}
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
        <div
          className="grid grid-cols-2 gap-2"
          role="radiogroup"
          aria-label="Chat input Enter behaviour"
        >
          <button
            ref={(el) => {
              chatRefs.current[0] = el;
            }}
            type="button"
            role="radio"
            aria-checked={chatEnterSubmits}
            tabIndex={chatEnterSubmits ? 0 : -1}
            onClick={() => setChatEnterSubmits(true)}
            onKeyDown={(e) =>
              moveRadioFocus(e, chatRefs.current, currentChatIndex, (next) =>
                setChatEnterSubmits(next === 0),
              )
            }
            className={`flex flex-col items-start gap-1 rounded border p-3 text-left transition-colors focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent ${
              chatEnterSubmits
                ? "border-accent bg-accent-soft/30 ring-1 ring-accent/40"
                : "border-border-default bg-surface-1 hover:bg-surface-2"
            }`}
          >
            <span className="text-fg-default font-medium">Enter submits</span>
            <span className="text-xs text-fg-subtle">
              Enter sends the message. Shift+Enter inserts a newline.
            </span>
          </button>
          <button
            ref={(el) => {
              chatRefs.current[1] = el;
            }}
            type="button"
            role="radio"
            aria-checked={!chatEnterSubmits}
            tabIndex={!chatEnterSubmits ? 0 : -1}
            onClick={() => setChatEnterSubmits(false)}
            onKeyDown={(e) =>
              moveRadioFocus(e, chatRefs.current, currentChatIndex, (next) =>
                setChatEnterSubmits(next === 0),
              )
            }
            className={`flex flex-col items-start gap-1 rounded border p-3 text-left transition-colors focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-accent ${
              !chatEnterSubmits
                ? "border-accent bg-accent-soft/30 ring-1 ring-accent/40"
                : "border-border-default bg-surface-1 hover:bg-surface-2"
            }`}
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
