import { LiveDot, type LiveDotTone } from "@/components/ui/LiveDot";

interface Props {
  // Mirrors useRunStore.wsState — "open" | "connecting" | "reconnecting" |
  // "closed" plus any future values; rendered as a coloured dot with a
  // tooltip on hover so the live status stays unobtrusive.
  state: string;
}

interface Style {
  tone: LiveDotTone;
  label: string;
  pulse: boolean;
}

function styleFor(state: string): Style {
  switch (state) {
    case "open":
      return { tone: "success", label: "Connected", pulse: false };
    case "connecting":
      return { tone: "info", label: "Connecting…", pulse: true };
    case "reconnecting":
      return { tone: "warning", label: "Reconnecting…", pulse: true };
    case "closed":
      return { tone: "danger", label: "Disconnected", pulse: false };
    default:
      return { tone: "neutral", label: state || "Unknown", pulse: false };
  }
}

export default function WSStatusDot({ state }: Props) {
  const { tone, label, pulse } = styleFor(state);
  return (
    // No aria-label here: ARIA in HTML prohibits an accessible name on a
    // roleless span (axe aria-prohibited-attr, #2767). LiveDot's label
    // carries the state as sr-only text under role="status" instead.
    <span className="inline-flex items-center" title={`WebSocket: ${label}`}>
      <LiveDot tone={tone} size="md" pulse={pulse} label={`WebSocket: ${label}`} />
    </span>
  );
}
