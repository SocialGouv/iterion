import type { CodemodeReport, RunEvent, ToolBatchSlot } from "@/api/runs";

// The Tools tab's model of a node's tool calls, built from the run's events
// alone: a pure function of what the events carry, kept apart from the
// components that render it.

export interface ToolCall {
  seq: number;
  toolName: string;
  isError: boolean;
  duration?: number;
  rawData: Record<string, unknown> | undefined;
  // input/output: present when the call fit inline OR carries a head
  // preview from the sidecar blob. Always renderable as the initial
  // payload view.
  input?: string;
  output?: string;
  // {input,output}Ref: when set, the full body lives in a sidecar blob
  // addressable via fetchToolBlob(runId, ref, kind). The studio renders
  // a "Load more" affordance below the preview that paginates through
  // the rest of the bytes (infinite scroll).
  inputRef?: string;
  outputRef?: string;
  // {input,output}Size: total byte size of the original body. Used to
  // size progress affordances and decide whether to offer Load more.
  inputSize?: number;
  outputSize?: number;
  errorMsg?: string;
  toolUseID?: string;
  // The call that made this one, when the event names it — a codemode
  // program's nested call names its program's call.
  parentToolUseID?: string;
  batch?: ToolBatchSlot;
  // A codemode program's report, from its completion event.
  codemode?: CodemodeReport;
  // The seq of the call's tool_started, when the window holds it.
  startSeq?: number;
  // A call whose start is in the window and whose completion is not yet:
  // only a program with nested calls gets a card in that state, so its
  // calls have a home while it runs.
  running?: boolean;
  // The calls this one made, keyed by their parent_tool_use_id.
  nested?: ToolCall[];
  // A start the window cannot pair with one completion — its id names
  // several open calls — shown on its own rather than matched to a guess.
  unpaired?: boolean;
  // Who emitted the event when it is not the loop that ran the call:
  // "launcher" on the sandbox launcher's own refusal of a forwarded call,
  // whose id and parent are the runner's claim.
  emittedBy?: string;
}

// The event field marking the launcher's own refusal of a forwarded call
// (pkg/backend/model ToolEventEmittedByLauncher).
export const LAUNCHER_EMITTED = "launcher";

// pickInlinePayload returns whatever string form of the tool call's
// `kind` payload is available on the merged event data: the full inline
// body if the call fit under the threshold, otherwise the 4 KB head
// preview the backend included alongside the sidecar blob ref. Returns
// undefined when neither is set (e.g. an empty input/output).
export function pickInlinePayload(
  data: Record<string, unknown>,
  kind: "input" | "output",
): string | undefined {
  const inline = data[kind];
  if (typeof inline === "string") return inline;
  if (inline !== undefined) return safeJSON(inline);
  const preview = data[`${kind}_preview`];
  if (typeof preview === "string") return preview;
  return undefined;
}

// pickRef returns the sidecar blob ref (= tool_use_id) when present.
// The presence of a ref tells the studio "there's more to fetch beyond
// the preview" — render the Load more affordance.
export function pickRef(
  data: Record<string, unknown>,
  kind: "input" | "output",
): string | undefined {
  const ref = data[`${kind}_ref`];
  return typeof ref === "string" && ref !== "" ? ref : undefined;
}

// pickSize returns the total byte size of the original tool payload as
// declared on the event (`{kind}_size`). Used to size Load more progress
// affordances. Returns undefined when the event predates this plumbing.
export function pickSize(
  data: Record<string, unknown>,
  kind: "input" | "output",
): number | undefined {
  const size = data[`${kind}_size`];
  return typeof size === "number" ? size : undefined;
}

function nonEmptyString(v: unknown): string | undefined {
  return typeof v === "string" && v !== "" ? v : undefined;
}

export function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function batchOf(v: unknown): ToolBatchSlot | undefined {
  if (!isRecord(v)) return undefined;
  const { first, size, index } = v;
  return typeof first === "number" && typeof size === "number" && typeof index === "number"
    ? { first, size, index }
    : undefined;
}

function codemodeOf(v: unknown): CodemodeReport | undefined {
  if (!isRecord(v) || !isRecord(v["limits"])) return undefined;
  return v as unknown as CodemodeReport;
}

function toolCallFrom(
  data: Record<string, unknown>,
  seq: number,
  isError: boolean,
  running: boolean,
  startSeq: number | undefined,
): ToolCall {
  const toolName =
    (data["tool_name"] as string) ?? (data["tool"] as string) ?? "unknown";
  return {
    seq,
    toolName,
    isError,
    duration:
      typeof data["duration_ms"] === "number" ? (data["duration_ms"] as number) : undefined,
    rawData: data,
    input: pickInlinePayload(data, "input"),
    output: pickInlinePayload(data, "output"),
    inputRef: pickRef(data, "input"),
    outputRef: pickRef(data, "output"),
    inputSize: pickSize(data, "input"),
    outputSize: pickSize(data, "output"),
    errorMsg: isError
      ? (data["error"] as string) ?? (data["message"] as string)
      : undefined,
    toolUseID: nonEmptyString(data["tool_use_id"]),
    parentToolUseID: nonEmptyString(data["parent_tool_use_id"]),
    batch: batchOf(data["batch"]),
    codemode: codemodeOf(data["codemode"]),
    startSeq,
    running: running || undefined,
    emittedBy: nonEmptyString(data["emitted_by"]),
  };
}

// The order a call is shown in among its siblings: when it started, else
// when it finished. Display only — grouping never reads it.
function orderKey(c: ToolCall): number {
  return c.startSeq ?? c.seq;
}

function byOrder(a: ToolCall, b: ToolCall): number {
  return orderKey(a) - orderKey(b) || a.seq - b.seq;
}

// groupToolCalls merges `tool_started` (carrying the JSON input) with
// `tool_called` / `tool_error` (carrying duration, error, and the tool's
// output result) by `tool_use_id`, then hangs each call under the call its
// `parent_tool_use_id` names — a codemode program's nested calls under the
// program. Pure: the grouping is a function of the ids the events carry,
// never of the order the array holds them in, so a nested call that
// completed before its program lands in the same place as one that
// completed after.
//
// It never drops an event. A call without a parent id stays top-level: a
// call the model made itself, and every event older than the field — those
// are never regrouped by the shape of an id or by where they sit in the
// stream. A call whose parent is absent from the window, or names more than
// one call, or whose chain of parents loops back on itself (only a forged
// stream can), is shown at the top level too: every call is reachable from
// a top-level card.
//
// An id is not unique in every window: a provider that numbers its calls
// per turn (`call_0`) reuses one across steps. A completion pairs with a
// start of its id only when exactly one start of that id is still open
// BEFORE it in the run's sequence — a call's start precedes its completion,
// and every pairing event is the loop's own, applied in order (the launcher's
// refusal never pairs). Otherwise nothing pairs: a turn-1 completion never
// takes turn 2's start, and when several starts are open the starts and the
// completion each get a card of their own, never a guessed pair.
//
// The launcher's own refusal of a forwarded call (`emitted_by: launcher`)
// carries an id and a parent the RUNNER claimed. It is a card of its own,
// with its own text: never paired with a start, never merged with the
// runner's event for the call, never a parent. It sits under the parent it
// names when that parent resolves, at the top level otherwise.
//
// A call whose start lacks a completion is not shown (the in-flight footer
// owns that), except a program some nested call names, which gets a running
// card so its calls have a home, and a start an ambiguous id left unpaired.
export function groupToolCalls(matching: RunEvent[]): ToolCall[] {
  const events = [...matching].sort((a, b) => a.seq - b.seq);
  const startsByID = new Map<string, RunEvent[]>();
  for (const e of events) {
    if (e.type !== "tool_started") continue;
    const id = nonEmptyString(e.data?.["tool_use_id"]);
    if (id) startsByID.set(id, [...(startsByID.get(id) ?? []), e]);
  }
  const paired = new Set<RunEvent>();
  const ambiguous = new Set<RunEvent>();
  const startFor = (id: string | undefined, seq: number): RunEvent | undefined => {
    const starts = id ? startsByID.get(id) : undefined;
    if (!starts) return undefined;
    const open = starts.filter((st) => st.seq < seq && !paired.has(st));
    if (open.length === 1) return open[0];
    if (open.length > 1) for (const st of open) ambiguous.add(st);
    return undefined;
  };

  const calls: ToolCall[] = [];
  for (const e of events) {
    if (e.type !== "tool_called" && e.type !== "tool_error") continue;
    const data = e.data ?? {};
    if (data["emitted_by"] === LAUNCHER_EMITTED) {
      calls.push(toolCallFrom({ ...data }, e.seq, e.type === "tool_error", false, undefined));
      continue;
    }
    const start = startFor(nonEmptyString(data["tool_use_id"]), e.seq);
    if (start) paired.add(start);
    // Post-execution event wins for duration/error; the pre-execution
    // event provides the input payload for structured rendering.
    calls.push(
      toolCallFrom({ ...(start?.data ?? {}), ...data }, e.seq, e.type === "tool_error", false, start?.seq),
    );
  }
  const named = new Set<string>();
  for (const c of calls) if (c.parentToolUseID && !c.emittedBy) named.add(c.parentToolUseID);
  for (const [id, starts] of startsByID) {
    for (const st of starts) {
      if (paired.has(st)) continue;
      if (ambiguous.has(st)) {
        calls.push({ ...toolCallFrom({ ...(st.data ?? {}) }, st.seq, false, false, st.seq), unpaired: true });
      } else if (named.has(id)) {
        calls.push(toolCallFrom({ ...(st.data ?? {}) }, st.seq, false, true, st.seq));
      }
    }
  }

  // A launcher refusal is never a parent: its id is the runner's claim.
  const byID = new Map<string, ToolCall[]>();
  for (const c of calls) {
    if (c.toolUseID && !c.emittedBy) byID.set(c.toolUseID, [...(byID.get(c.toolUseID) ?? []), c]);
  }
  const parentOf = (c: ToolCall): ToolCall | undefined => {
    const candidates = c.parentToolUseID ? byID.get(c.parentToolUseID) : undefined;
    const parent = candidates?.length === 1 ? candidates[0] : undefined;
    return parent === c ? undefined : parent;
  };
  // Rooted: the chain of parents ends at a call with none. A chain that
  // loops never does, and every call on or below the loop goes to the top.
  const rooted = (c: ToolCall): boolean => {
    const seen = new Set<ToolCall>();
    for (let at: ToolCall | undefined = c; at; at = parentOf(at)) {
      if (seen.has(at)) return false;
      seen.add(at);
    }
    return true;
  };
  const top: ToolCall[] = [];
  for (const c of calls) {
    const parent = parentOf(c);
    if (parent && rooted(c)) {
      (parent.nested ??= []).push(c);
    } else {
      top.push(c);
    }
  }
  for (const c of calls) c.nested?.sort(byOrder);
  return top.sort((a, b) => a.seq - b.seq);
}


// A program's nested calls as they are listed: a single call alone, the
// entries of one `call.parallel` batch together, in their batch order.
export type NestedItem =
  | { kind: "call"; call: ToolCall }
  | { kind: "batch"; first: number; size: number; calls: ToolCall[] };

export function nestedItems(calls: ToolCall[]): NestedItem[] {
  const items: NestedItem[] = [];
  const batches = new Map<number, Extract<NestedItem, { kind: "batch" }>>();
  for (const c of [...calls].sort(byOrder)) {
    if (!c.batch) {
      items.push({ kind: "call", call: c });
      continue;
    }
    let group = batches.get(c.batch.first);
    if (!group) {
      group = { kind: "batch", first: c.batch.first, size: c.batch.size, calls: [] };
      batches.set(c.batch.first, group);
      items.push(group);
    }
    group.calls.push(c);
  }
  for (const g of batches.values()) {
    g.calls.sort((a, b) => (a.batch?.index ?? 0) - (b.batch?.index ?? 0));
  }
  return items;
}

function safeJSON(v: unknown): string {
  try {
    return JSON.stringify(v, null, 2);
  } catch {
    return String(v);
  }
}
