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

// positiveNumber parses a form string into a positive finite number.
// Empty, non-numeric, zero and negative all mean "inherit" — the server
// treats zero as nobody-chose and refuses a negative, so neither is sent.
function positiveNumber(s: string): number | undefined {
  const t = s.trim();
  if (!t) return undefined;
  const n = Number(t);
  return Number.isFinite(n) && n > 0 ? n : undefined;
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
  if (toolCalls !== undefined) bounds.tool_calls = Math.floor(toolCalls);
  const wall = fields.wall.trim();
  if (wall) bounds.wall = wall;
  const cpu = fields.cpu.trim();
  if (cpu) bounds.cpu = cpu;
  const memory = positiveNumber(fields.memory);
  if (memory !== undefined) bounds.memory = Math.floor(memory);
  const script = positiveNumber(fields.scriptBytes);
  if (script !== undefined) bounds.script_bytes = Math.floor(script);
  const result = positiveNumber(fields.resultBytes);
  if (result !== undefined) bounds.result_bytes = Math.floor(result);
  const toolResult = positiveNumber(fields.toolResultBytes);
  if (toolResult !== undefined) bounds.tool_result_bytes = Math.floor(toolResult);

  if (Object.keys(bounds).length > 0) out.codemode_bounds = bounds;
  return out;
}

// boundsSet reports whether the operator changed any bounds field — what the
// section uses to open its disclosure and count its set fields.
export function boundsSet(fields: CodemodeFieldValues): boolean {
  return Object.values(fields).some((v) => v.trim() !== "");
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
    if (n !== undefined && n > max) over.push(name);
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
