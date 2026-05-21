import { ThinkingIndicator } from "@/components/ui/ThinkingIndicator";
import { GENERIC_THINKING_WORDS } from "@/lib/thinkingWords";

// ThinkingFooter is the Virtuoso footer slot that fires while the run
// is mid-turn (no synchronous tool in flight). The actual animation
// primitive lives in `@/components/ui/ThinkingIndicator` so the same
// look-and-feel can be reused by WhatsNext banners and PreFlightPanel.
export function ThinkingFooter({ active }: { active: boolean }) {
  return <ThinkingIndicator words={GENERIC_THINKING_WORDS} active={active} />;
}
