const DAY_NAMES = [
  "Sunday",
  "Monday",
  "Tuesday",
  "Wednesday",
  "Thursday",
  "Friday",
  "Saturday",
];

// humanizeCron renders the common 5-field cron shapes as a short English
// hint ("0 3 * * 1" → "every Monday at 03:00"). Deliberately conservative:
// anything outside the fixed-minute/hour + every-N + single day-of-week/
// day-of-month forms returns null and the caller shows the raw expression
// alone — a missing hint beats a wrong translation.
export function humanizeCron(expr: string): string | null {
  // Cron lines may carry a `CRON_TZ=<zone> ` prefix (the crontab timezone
  // vocabulary the scheduler accepts) — humanize the schedule part and
  // append the zone.
  const tzMatch = /^\s*CRON_TZ=(\S+)\s+(.*)$/.exec(expr);
  if (tzMatch) {
    const base = humanizeCron(tzMatch[2] ?? "");
    return base ? `${base} (${tzMatch[1]})` : null;
  }
  const fields = expr.trim().split(/\s+/);
  if (fields.length !== 5) return null;
  // Length checked above — safe to narrow the destructuring.
  const [min, hour, dom, mon, dow] = fields as [string, string, string, string, string];
  if (mon !== "*") return null;

  const num = (s: string): number | null => (/^\d+$/.test(s) ? Number(s) : null);
  const everyN = (s: string): number | null => {
    const m = /^\*\/(\d+)$/.exec(s);
    return m ? Number(m[1]) : null;
  };
  const dayName = (s: string): string | null => {
    const n = num(s);
    // Both 0 and 7 mean Sunday in the 5-field vocabulary.
    if (n !== null) return n >= 0 && n <= 7 ? (DAY_NAMES[n % 7] ?? null) : null;
    const idx = DAY_NAMES.findIndex(
      (d) => d.slice(0, 3).toLowerCase() === s.toLowerCase(),
    );
    return idx >= 0 ? (DAY_NAMES[idx] ?? null) : null;
  };
  const pad = (n: number) => String(n).padStart(2, "0");

  const m = num(min);
  const h = num(hour);

  // Minute-cadence forms: "* * * * *" / "*/N * * * *".
  if (hour === "*" && dom === "*" && dow === "*") {
    if (min === "*") return "every minute";
    const n = everyN(min);
    if (n !== null) return n === 1 ? "every minute" : `every ${n} minutes`;
    if (m !== null) return `hourly at :${pad(m)}`;
    return null;
  }
  if (m === null) return null;

  // Hour-cadence forms: "M */N * * *".
  if (dom === "*" && dow === "*") {
    const n = everyN(hour);
    if (n !== null) {
      return n === 1 ? `hourly at :${pad(m)}` : `every ${n} hours at :${pad(m)}`;
    }
  }
  if (h === null) return null;
  const at = `${pad(h)}:${pad(m)}`;

  if (dom === "*" && dow === "*") return `daily at ${at}`;
  // Weekly: a day range ("1-5"), a single day, or a comma list of days.
  if (dom === "*") {
    const range = /^([^,-]+)-([^,-]+)$/.exec(dow);
    if (range) {
      const from = dayName(range[1] ?? "");
      const to = dayName(range[2] ?? "");
      if (from === null || to === null) return null;
      return `every ${from} to ${to} at ${at}`;
    }
    const days = dow.split(",").map(dayName);
    if (days.some((d) => d === null)) return null;
    return `every ${days.join(", ")} at ${at}`;
  }
  // Monthly on a fixed day.
  if (dow === "*") {
    const d = num(dom);
    if (d === null || d < 1 || d > 31) return null;
    return `monthly on day ${d} at ${at}`;
  }
  return null;
}
