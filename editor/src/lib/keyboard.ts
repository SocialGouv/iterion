// isTypingTarget returns true when the keyboard event's target is an
// editable surface — input, textarea, select, or any contenteditable
// element. Shared by every window-scoped keyboard handler so a single
// rule keeps text fields immune from app-wide shortcuts.
export function isTypingTarget(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  const tag = target.tagName;
  if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT") return true;
  if (target.isContentEditable) return true;
  return false;
}
