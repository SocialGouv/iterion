// Pure payload-building logic for the Launch form's codemode override
// (CM-5f, ADR-165) — kept separate from CodemodeSection.tsx so it can be
// unit-tested without rendering, the budgetPayload precedent.
//
// The form keeps every field as a raw input string. buildCodemodePayload
// converts the set to the wire shape of CreateRunRequest (`codemode` +
// `codemode_bounds`, pkg/server/runs_launch.go) — or omits a piece the
// operator did not set, so the run inherits what the .bot declared.
//
// The RULE, in one line: an unset field never travels. A zero is "nobody
// chose" to the resolver's field-by-field ladder, and a negative is refused
// by the server with a 400 — the form sends neither.

import type { CodemodeBoundsOverride, CodemodeLimits } from "@/api/runs";

export interface CodemodeFieldValues {
  toolCalls: string;
  wall: string;
  cpu: string;
  memory: string;
  scriptBytes: string;
  resultBytes: string;
  toolResultBytes: string;
}

export const emptyCodemodeFieldValues: CodemodeFieldValues = {
  toolCalls: "",
  wall: "",
  cpu: "",
  memory: "",
  scriptBytes: "",
  resultBytes: "",
  toolResultBytes: "",
};

export interface CodemodePayload {
  codemode?: string;
  codemode_bounds?: CodemodeBoundsOverride;
}

// positiveNumber parses a form string into a positive INTEGER — every bound
// it feeds is a count or a size, and the floor is applied HERE, before the
// positivity test: a "0.5" floors to 0, and a payload carrying `tool_calls:
// 0` would be the exact zero this module promises never to send (the server
// reads it as nobody-chose while the form claims an override — found by the
// revi gate). Empty, non-numeric, zero, negative and sub-1 all mean
// "inherit".
function positiveNumber(s: string): number | undefined {
  const t = s.trim();
  if (!t) return undefined;
  const n = Number(t);
  if (!Number.isFinite(n)) return undefined;
  const floored = Math.floor(n);
  return floored > 0 ? floored : undefined;
}

// buildCodemodePayload maps the form state to the request fields. Returns an
// empty object when the operator chose nothing at all, so the caller can
// spread it without inventing a key.
export function buildCodemodePayload(
  mode: string,
  fields: CodemodeFieldValues,
): CodemodePayload {
  const out: CodemodePayload = {};
  const m = mode.trim();
  if (m) out.codemode = m;

  const bounds: CodemodeBoundsOverride = {};
  const toolCalls = positiveNumber(fields.toolCalls);
  if (toolCalls !== undefined) bounds.tool_calls = toolCalls;
  // Durations travel VERBATIM (the server owns the parse and 400s a value it
  // cannot read, naming the field) — except a value that PARSES TO ZERO:
  // "0s" reaches the resolver as the unset it looks like, silently reverting
  // to the bot while the form claims an override. Dropped here, like the
  // numeric zero (revi gate, same class).
  const wall = fields.wall.trim();
  if (wall && !droppableDuration(wall)) bounds.wall = wall;
  const cpu = fields.cpu.trim();
  if (cpu && !droppableDuration(cpu)) bounds.cpu = cpu;
  const memory = positiveNumber(fields.memory);
  if (memory !== undefined) bounds.memory = memory;
  const script = positiveNumber(fields.scriptBytes);
  if (script !== undefined) bounds.script_bytes = script;
  const result = positiveNumber(fields.resultBytes);
  if (result !== undefined) bounds.result_bytes = result;
  const toolResult = positiveNumber(fields.toolResultBytes);
  if (toolResult !== undefined) bounds.tool_result_bytes = toolResult;

  if (Object.keys(bounds).length > 0) out.codemode_bounds = bounds;
  return out;
}

// boundsSet reports whether any bounds field holds a value AT ALL — the
// raw question ("did the operator type something"), which is what the
// section's disclosure hint uses. It is NOT "something travels": zero, a
// negative and junk are dropped by buildCodemodePayload, so a caller that
// wants to say "overriding the bot" asks the payload, not this.
export function boundsSet(fields: CodemodeFieldValues): boolean {
  return Object.values(fields).some((v) => v.trim() !== "");
}

// belowMemoryFloor reports whether the operator typed a memory bound the
// child cannot start at: Clamp RAISES it to the floor at spawn, so the run
// would not use the number on screen (found by the adversarial round — the
// one divergence the form could show silently).
export function belowMemoryFloor(
  fields: CodemodeFieldValues,
  limits: CodemodeLimits | undefined,
): number | undefined {
  const floor = limits?.memory_floor;
  if (!floor) return undefined;
  const n = positiveNumber(fields.memory);
  if (n === undefined) return undefined;
  return Math.floor(n) < floor ? floor : undefined;
}

// droppableDuration reports whether a duration field holds a value that
// parses to ZERO (or less): the resolver reads that as "nobody chose", so
// sending it would promise an override no run gets. An UNPARSABLE duration
// is not droppable here — it travels and the server names it in a 400.
export function droppableDuration(raw: string): boolean {
  const secs = parseDurationSeconds(raw);
  return secs !== undefined && secs <= 0;
}

// unreadableFields names the fields holding a non-empty value the payload
// cannot carry (non-numeric, zero, negative, a zero duration): the form
// marks them aria-invalid rather than letting them read as a silent
// inherit.
export function unreadableFields(fields: CodemodeFieldValues): string[] {
  const numeric: Array<[string, string]> = [
    ["tool_calls", fields.toolCalls],
    ["memory", fields.memory],
    ["script_bytes", fields.scriptBytes],
    ["result_bytes", fields.resultBytes],
    ["tool_result_bytes", fields.toolResultBytes],
  ];
  const out: string[] = [];
  for (const [name, raw] of numeric) {
    if (raw.trim() !== "" && positiveNumber(raw) === undefined) out.push(name);
  }
  for (const [name, raw] of [["wall", fields.wall], ["cpu", fields.cpu]] as const) {
    if (raw.trim() !== "" && droppableDuration(raw)) out.push(name);
  }
  return out;
}

// parseDurationSeconds reads the Go duration spellings the DSL accepts for
// `wall`/`cpu` ("30s", "5m", "1h30m", "500ms") into seconds. Returns
// undefined for anything else — the form then shows the value as-is rather
// than pretend to understand it (the server owns the authoritative parse and
// answers a 400 naming the field).
export function parseDurationSeconds(raw: string): number | undefined {
  const t = raw.trim();
  if (!t) return undefined;
  const re = /(\d+(?:\.\d+)?)(ms|s|m|h)/g;
  let total = 0;
  let matched = 0;
  let m: RegExpExecArray | null;
  while ((m = re.exec(t)) !== null) {
    const n = Number(m[1]);
    const unit = m[2];
    total +=
      unit === "h" ? n * 3600 : unit === "m" ? n * 60 : unit === "s" ? n : n / 1000;
    matched += m[0].length;
  }
  // Every character must have been consumed: "5m30" or "abc" is not a
  // duration, and a partial match would caption a wrong ceiling verdict.
  if (matched === 0 || matched !== t.length) return undefined;
  return total;
}

// overCeiling names the fields whose value exceeds the platform ceiling —
// the form's warning, never a block: the server clamps at spawn and reports
// through OnClamp, so refusing here would invent a second owner of the
// bound. An unparsable duration is not flagged (the server names it).
export function overCeiling(
  fields: CodemodeFieldValues,
  limits: CodemodeLimits | undefined,
): string[] {
  if (!limits) return [];
  const ceil = limits.ceiling;
  const over: string[] = [];
  const num = (name: string, raw: string, max: number | undefined) => {
    if (max === undefined) return;
    const n = positiveNumber(raw);
    // FLOORED, like the payload: a value in (ceiling, ceiling+1) travels as
    // the ceiling, and a warning there would name a divergence no run has.
    if (n !== undefined && Math.floor(n) > max) over.push(name);
  };
  const dur = (name: string, raw: string, max: string | undefined) => {
    if (!max) return;
    const got = parseDurationSeconds(raw);
    const cap = parseDurationSeconds(max);
    if (got !== undefined && cap !== undefined && got > cap) over.push(name);
  };
  num("tool_calls", fields.toolCalls, ceil.tool_calls);
  dur("wall", fields.wall, ceil.wall);
  dur("cpu", fields.cpu, ceil.cpu);
  num("memory", fields.memory, ceil.memory);
  num("script_bytes", fields.scriptBytes, ceil.script_bytes);
  num("result_bytes", fields.resultBytes, ceil.result_bytes);
  num("tool_result_bytes", fields.toolResultBytes, ceil.tool_result_bytes);
  return over;
}
