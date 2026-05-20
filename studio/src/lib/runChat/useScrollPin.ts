import { useCallback, useEffect, useRef } from "react";

// useScrollPin keeps a scroll container glued to its bottom as long
// as the user is already near the bottom. Extracted from the chat
// transcripts (generic RunConversationView + whats-next ChatTranscript)
// so the 48px threshold + ResizeObserver teardown live in one place.
//
// Usage:
//   const { scrollRef, endRef, onScroll } = useScrollPin([messages.length]);
//   return (
//     <div ref={scrollRef} onScroll={onScroll}>
//       {...rows}
//       <div ref={endRef} />
//     </div>
//   );
//
// The deps argument controls when to attempt a re-pin: pass anything
// whose change should trigger a "stick to bottom" decision (typically
// `messages.length`). The ResizeObserver covers in-place height changes
// the deps array misses (a textarea growing, a form expanding).
//
// The 48px threshold is large enough to survive brief smooth-scroll
// overshoot but small enough that the user only has to nudge up once
// to escape auto-follow.
export function useScrollPin<T>(deps: ReadonlyArray<T>) {
  const scrollRef = useRef<HTMLDivElement | null>(null);
  const endRef = useRef<HTMLDivElement | null>(null);
  const atBottomRef = useRef(true);

  const onScroll = useCallback(() => {
    const el = scrollRef.current;
    if (!el) return;
    const distanceFromBottom = el.scrollHeight - el.scrollTop - el.clientHeight;
    atBottomRef.current = distanceFromBottom < 48;
  }, []);

  useEffect(() => {
    if (!atBottomRef.current) return;
    endRef.current?.scrollIntoView({ behavior: "smooth", block: "end" });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);

  useEffect(() => {
    const el = scrollRef.current;
    if (!el || typeof ResizeObserver === "undefined") return;
    const obs = new ResizeObserver(() => {
      if (!atBottomRef.current) return;
      endRef.current?.scrollIntoView({ behavior: "auto", block: "end" });
    });
    obs.observe(el);
    return () => obs.disconnect();
  }, []);

  return { scrollRef, endRef, onScroll };
}
