// MAX_LOG_BYTES caps the in-memory log tail so a verbose run doesn't
// bloat the React heap. Older bytes fall off the front; the start
// offset advances accordingly. Matches the backend ring of 1 MiB so
// the WS replay window stays consistent.
export const MAX_LOG_BYTES = 1 << 20;
// Truncate down to LOG_TRIM_TARGET (75% of cap) instead of the cap
// itself so we don't pay an O(N) slice on every appended chunk once
// the cap is reached — amortises the copy to one trim per ~256 KiB.
export const LOG_TRIM_TARGET = (MAX_LOG_BYTES * 3) >> 2;

// utf8Len returns the number of UTF-8 *bytes* a string encodes to, NOT
// its UTF-16 code-unit length (`String.prototype.length`). Used to keep
// the log byte cursor (RunLogState.nextByte) aligned with the backend,
// which tracks every log offset in bytes. Allocation-free so the
// one-shot ~1 MiB snapshot on tab open doesn't churn a Uint8Array.
export function utf8Len(s: string): number {
  let bytes = 0;
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (c < 0x80) bytes += 1;
    else if (c < 0x800) bytes += 2;
    else if (c >= 0xd800 && c <= 0xdbff) {
      // High surrogate → a code point ≥ U+10000 (4 bytes); consume the
      // paired low surrogate so it isn't counted again.
      bytes += 4;
      i++;
    } else bytes += 3;
  }
  return bytes;
}

// sliceFromByteOffset returns `s` with its leading `byteSkip` UTF-8 bytes
// removed, cutting on a code-point boundary. The backend keys log offsets in
// bytes while a JS string is indexed in UTF-16 code units, so overlap/trim
// arithmetic on the log tail must convert a byte count into the matching
// code-unit slice — using String.prototype.slice(byteSkip) directly corrupts
// any tail containing multi-byte glyphs. If byteSkip lands inside a multi-byte
// code point (a chunk split mid-character) the whole code point is skipped, so
// the result never begins with a partial character. byteSkip ≤ 0 returns `s`;
// byteSkip ≥ its byte length returns "".
export function sliceFromByteOffset(s: string, byteSkip: number): string {
  if (byteSkip <= 0) return s;
  let bytes = 0;
  let unit = 0; // UTF-16 code-unit index reached
  for (let i = 0; i < s.length; i++) {
    if (bytes >= byteSkip) break;
    const c = s.charCodeAt(i);
    if (c < 0x80) bytes += 1;
    else if (c < 0x800) bytes += 2;
    else if (c >= 0xd800 && c <= 0xdbff) {
      bytes += 4;
      i++; // consume paired low surrogate
    } else bytes += 3;
    unit = i + 1;
  }
  return s.slice(unit);
}

// sliceToByteOffset returns the leading `byteLen` UTF-8 bytes of `s` as a
// string, cutting on a code-point boundary — the head-side twin of
// sliceFromByteOffset. The replay scrubber clamps the log to an absolute
// byte position (Event.log_offset), so the visible slice must be cut by
// bytes, not UTF-16 code units. If byteLen lands inside a multi-byte code
// point, the partial code point is excluded, so the result never ends with
// a broken character. byteLen <= 0 returns ""; byteLen ≥ the string's byte
// length returns `s`.
export function sliceToByteOffset(s: string, byteLen: number): string {
  if (byteLen <= 0) return "";
  let bytes = 0;
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    let step: number;
    let next = i + 1;
    if (c < 0x80) step = 1;
    else if (c < 0x800) step = 2;
    else if (c >= 0xd800 && c <= 0xdbff) {
      step = 4;
      next = i + 2; // paired low surrogate
    } else step = 3;
    if (bytes + step > byteLen) return s.slice(0, i);
    bytes += step;
    i = next - 1;
  }
  return s;
}

// clampLogBody resolves what the log panel should render at a replay
// clamp position. `text` covers bytes [start, …) of the run's log
// stream; `prefix` (when loaded and still aligned: prefix.until ===
// start) covers the evicted head [0, start). clampToBytes is the
// absolute byte position to clamp at (null/undefined = live, no
// clamp). All cuts are byte-accurate — Event.log_offset counts UTF-8
// bytes while JS strings index UTF-16 code units.
export function clampLogBody(
  text: string,
  start: number,
  prefix: { until: number; text: string } | null,
  clampToBytes: number | null | undefined,
): string {
  if (clampToBytes === null || clampToBytes === undefined) return text;
  const pfx = prefix && prefix.until === start ? prefix.text : null;
  if (clampToBytes <= start) {
    // Scrub position falls inside the evicted head: only the fetched
    // prefix can show it. Empty while the fetch is in flight/failed.
    return pfx ? sliceToByteOffset(pfx, clampToBytes) : "";
  }
  return (pfx ?? "") + sliceToByteOffset(text, clampToBytes - start);
}
