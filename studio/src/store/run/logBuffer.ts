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
