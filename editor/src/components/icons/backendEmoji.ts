// Per-backend glyph used next to the backend name in node meta rows.
// `claw` is iterion's in-process backend — the crab is a play on "claws"
// and gives the internal agent the same at-a-glance recognition that
// branded delegates (claude_code, codex) get from their provider icons.

const BACKEND_EMOJI: Record<string, string> = {
  claw: "\u{1F980}",        // 🦀
  claude_code: "\u{1F916}", // 🤖
  codex: "\u{1F419}",       // 🐙
  direct: "\u{1F310}",      // 🌐
};

const FALLBACK = "\u{1F9F0}"; // 🧰 (generic toolbox)

export function backendEmoji(backend: string | undefined): string {
  if (!backend) return FALLBACK;
  return BACKEND_EMOJI[backend] ?? FALLBACK;
}
