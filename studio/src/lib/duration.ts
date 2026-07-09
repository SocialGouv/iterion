// Millisecond weight of each Go time.Duration unit.
const UNIT_MS: Record<string, number> = {
  ns: 1e-6,
  us: 1e-3,
  "µs": 1e-3, // U+00B5 micro sign
  "μs": 1e-3, // U+03BC greek mu (Go emits U+00B5, accept both)
  ms: 1,
  s: 1000,
  m: 60_000,
  h: 3_600_000,
};

// parseGoDuration parses a Go time.Duration string ("30m", "1h30m",
// "1.5h", "500ms") into milliseconds. Returns null on empty or
// unparseable input so a caller can distinguish "no cap set" (null) from
// a real zero. The whole string must be valid unit runs — a stray
// trailing token ("30x") rejects rather than silently truncating.
export function parseGoDuration(s?: string | null): number | null {
  if (!s) return null;
  let rest = s.trim();
  if (rest === "") return null;
  let sign = 1;
  if (rest[0] === "+" || rest[0] === "-") {
    if (rest[0] === "-") sign = -1;
    rest = rest.slice(1);
  }
  const re = /(\d+(?:\.\d+)?)(ns|us|µs|μs|ms|s|m|h)/g;
  let total = 0;
  let consumed = 0;
  let m: RegExpExecArray | null;
  while ((m = re.exec(rest)) !== null) {
    const full = m[0];
    const num = m[1];
    const unit = m[2];
    if (full === undefined || num === undefined || unit === undefined) continue;
    const weight = UNIT_MS[unit];
    if (weight === undefined) continue;
    consumed += full.length;
    total += parseFloat(num) * weight;
  }
  if (consumed === 0 || consumed !== rest.length) return null;
  return sign * total;
}
