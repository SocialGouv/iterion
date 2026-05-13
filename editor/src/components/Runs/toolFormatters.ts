// Per-tool input parsers for the run-view Tools tab.
//
// Tool calls land in `events.jsonl` as `tool_called` events whose
// `data.input` is either a JSON string (when serialized by the
// backend) or already a structured value. The Tools tab used to show
// only the tool name + duration — every parameter (file path, grep
// pattern, bash command, …) was hidden behind a collapsible JSON
// blob. This module produces a structured "fields" view so the most
// useful args are always visible on the card.

export interface ToolField {
  label: string;
  value: string;
  mono?: boolean;
}

export type TodoStatus = "pending" | "in_progress" | "completed";

export interface TodoItem {
  content: string;
  status: TodoStatus;
  activeForm?: string;
}

export interface ToolSummary {
  fields: ToolField[];
  // Structured payload for tools that benefit from a richer rendering
  // than label/value pairs. `todos` is populated by the TodoWrite parser
  // so the Tools tab can show the full task list (status + content) as
  // a visual checklist rather than a bare count.
  todos?: TodoItem[];
  // True when the input couldn't be parsed (non-JSON string, missing,
  // or empty object). Callers can fall back to the raw view.
  unparsed: boolean;
}

// Best-effort JSON parse. Inputs come from `event.data.input` which
// can be a string (serialized arg blob), an object (claude_code tool
// calls captured as structured maps), or undefined.
function toObject(input: unknown): Record<string, unknown> | null {
  if (input == null) return null;
  if (typeof input === "string") {
    const trimmed = input.trim();
    if (!trimmed) return null;
    if (!trimmed.startsWith("{") && !trimmed.startsWith("[")) return null;
    try {
      const parsed = JSON.parse(trimmed);
      return typeof parsed === "object" && parsed !== null
        ? (parsed as Record<string, unknown>)
        : null;
    } catch {
      return null;
    }
  }
  if (typeof input === "object") return input as Record<string, unknown>;
  return null;
}

function asString(v: unknown): string | undefined {
  if (typeof v === "string" && v.length > 0) return v;
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  return undefined;
}

function asNumber(v: unknown): number | undefined {
  if (typeof v === "number" && Number.isFinite(v)) return v;
  return undefined;
}

// Collapse newlines + indentation into single spaces and clip to
// `max` chars with an ellipsis. Used for bash commands, prompts and
// any long free-form text.
function singleLine(s: string, max = 140): string {
  const flat = s.replace(/\s+/g, " ").trim();
  if (flat.length <= max) return flat;
  return flat.slice(0, max - 1) + "…";
}

function clip(s: string, max = 140): string {
  if (s.length <= max) return s;
  return s.slice(0, max - 1) + "…";
}

// Build the per-tool field list. Each parser receives the parsed
// input object and returns the curated fields; an empty list means
// "no usable fields, render fallback".
type Parser = (input: Record<string, unknown>) => ToolField[];

const PARSERS: Record<string, Parser> = {
  Read: (i) => {
    const out: ToolField[] = [];
    const p = asString(i["file_path"]) ?? asString(i["path"]);
    if (p) out.push({ label: "path", value: p, mono: true });
    const offset = asNumber(i["offset"]);
    const limit = asNumber(i["limit"]);
    if (offset !== undefined) out.push({ label: "offset", value: String(offset), mono: true });
    if (limit !== undefined) out.push({ label: "limit", value: String(limit), mono: true });
    return out;
  },
  Edit: (i) => {
    const out: ToolField[] = [];
    const p = asString(i["file_path"]) ?? asString(i["path"]);
    if (p) out.push({ label: "path", value: p, mono: true });
    if (i["replace_all"] === true) out.push({ label: "replace_all", value: "true" });
    return out;
  },
  Write: (i) => {
    const out: ToolField[] = [];
    const p = asString(i["file_path"]) ?? asString(i["path"]);
    if (p) out.push({ label: "path", value: p, mono: true });
    const content = asString(i["content"]);
    if (content !== undefined) {
      out.push({ label: "bytes", value: String(content.length), mono: true });
    }
    return out;
  },
  Grep: (i) => {
    const out: ToolField[] = [];
    const pattern = asString(i["pattern"]);
    if (pattern) out.push({ label: "pattern", value: clip(pattern, 80), mono: true });
    const p = asString(i["path"]);
    if (p) out.push({ label: "path", value: p, mono: true });
    const glob = asString(i["glob"]);
    if (glob) out.push({ label: "glob", value: glob, mono: true });
    const mode = asString(i["output_mode"]);
    if (mode) out.push({ label: "mode", value: mode });
    const type = asString(i["type"]);
    if (type) out.push({ label: "type", value: type });
    return out;
  },
  Glob: (i) => {
    const out: ToolField[] = [];
    const pattern = asString(i["pattern"]);
    if (pattern) out.push({ label: "pattern", value: clip(pattern, 80), mono: true });
    const p = asString(i["path"]);
    if (p) out.push({ label: "path", value: p, mono: true });
    return out;
  },
  Bash: (i) => {
    const out: ToolField[] = [];
    const cmd = asString(i["command"]);
    if (cmd) out.push({ label: "command", value: singleLine(cmd, 140), mono: true });
    const desc = asString(i["description"]);
    if (desc) out.push({ label: "description", value: clip(desc, 80) });
    const timeout = asNumber(i["timeout"]);
    if (timeout !== undefined) out.push({ label: "timeout", value: `${timeout}ms`, mono: true });
    if (i["run_in_background"] === true) {
      out.push({ label: "background", value: "true" });
    }
    return out;
  },
  Task: (i) => {
    const out: ToolField[] = [];
    const sub = asString(i["subagent_type"]);
    if (sub) out.push({ label: "agent", value: sub, mono: true });
    const desc = asString(i["description"]);
    if (desc) out.push({ label: "description", value: clip(desc, 120) });
    return out;
  },
  WebFetch: (i) => {
    const out: ToolField[] = [];
    const url = asString(i["url"]);
    if (url) out.push({ label: "url", value: clip(url, 120), mono: true });
    const prompt = asString(i["prompt"]);
    if (prompt) out.push({ label: "prompt", value: clip(prompt, 120) });
    return out;
  },
  WebSearch: (i) => {
    const out: ToolField[] = [];
    const q = asString(i["query"]);
    if (q) out.push({ label: "query", value: clip(q, 140) });
    return out;
  },
  NotebookEdit: (i) => {
    const out: ToolField[] = [];
    const p = asString(i["notebook_path"]) ?? asString(i["path"]);
    if (p) out.push({ label: "path", value: p, mono: true });
    const cellId = asString(i["cell_id"]);
    if (cellId) out.push({ label: "cell", value: cellId, mono: true });
    const editMode = asString(i["edit_mode"]);
    if (editMode) out.push({ label: "mode", value: editMode });
    return out;
  },
  TodoWrite: (i) => {
    // The full list is surfaced via summary.todos (see formatToolCall);
    // here we add a single compact field with the per-status counts so
    // the header line still gives an at-a-glance picture before the
    // checklist is rendered below.
    const out: ToolField[] = [];
    const todos = i["todos"];
    if (!Array.isArray(todos)) return out;
    let pending = 0;
    let inProgress = 0;
    let done = 0;
    for (const raw of todos) {
      const status = (raw as { status?: string } | null)?.status;
      if (status === "in_progress") inProgress++;
      else if (status === "completed" || status === "done") done++;
      else pending++;
    }
    const parts: string[] = [`${todos.length} total`];
    if (inProgress) parts.push(`${inProgress} in progress`);
    if (done) parts.push(`${done} done`);
    if (pending) parts.push(`${pending} pending`);
    out.push({ label: "todos", value: parts.join(", "), mono: true });
    return out;
  },
};

// extractTodos pulls the well-formed TodoItem[] out of a TodoWrite input.
// Returns an empty array when the shape doesn't match, leaving the card
// to fall back to its standard fields-only rendering.
function extractTodos(input: Record<string, unknown>): TodoItem[] {
  const todos = input["todos"];
  if (!Array.isArray(todos)) return [];
  const out: TodoItem[] = [];
  for (const raw of todos) {
    if (raw === null || typeof raw !== "object") continue;
    const obj = raw as Record<string, unknown>;
    const content = typeof obj["content"] === "string" ? obj["content"] : "";
    if (!content) continue;
    let status: TodoStatus;
    const rawStatus = obj["status"];
    if (rawStatus === "in_progress") status = "in_progress";
    else if (rawStatus === "completed" || rawStatus === "done") status = "completed";
    else status = "pending";
    const activeForm =
      typeof obj["activeForm"] === "string" ? (obj["activeForm"] as string) : undefined;
    out.push({ content, status, activeForm });
  }
  return out;
}

// Generic fallback: pick the most informative top-level fields.
// Strings come first (truncated), numbers/booleans next, and we cap
// at 4 entries to keep the card compact.
function genericFields(input: Record<string, unknown>): ToolField[] {
  const out: ToolField[] = [];
  // Stable ordering = original key order (matches the producer's
  // emit order, which is meaningful for most tools).
  for (const key of Object.keys(input)) {
    if (out.length >= 4) break;
    const v = input[key];
    if (v == null) continue;
    if (typeof v === "string") {
      if (v.length === 0) continue;
      out.push({ label: key, value: singleLine(v, 100), mono: true });
    } else if (typeof v === "number" || typeof v === "boolean") {
      out.push({ label: key, value: String(v), mono: true });
    } else if (Array.isArray(v)) {
      out.push({ label: key, value: `[${v.length}]`, mono: true });
    } else if (typeof v === "object") {
      const keys = Object.keys(v as object);
      out.push({ label: key, value: `{${keys.length}}`, mono: true });
    }
  }
  return out;
}

// MCP tools come through as `mcp__<server>__<tool>`. We surface the
// server/tool split + delegate the rest to the generic parser.
function parseMCP(toolName: string, input: Record<string, unknown>): ToolField[] {
  const out: ToolField[] = [];
  const parts = toolName.slice("mcp__".length).split("__");
  const server = parts[0];
  const tool = parts.slice(1).join("__");
  if (server) out.push({ label: "server", value: server, mono: true });
  if (tool) out.push({ label: "tool", value: tool, mono: true });
  for (const f of genericFields(input)) {
    if (out.length >= 5) break;
    out.push(f);
  }
  return out;
}

export function formatToolCall(toolName: string, rawInput: unknown): ToolSummary {
  const obj = toObject(rawInput);
  if (!obj) {
    return { fields: [], unparsed: rawInput != null };
  }
  let fields: ToolField[];
  if (toolName.startsWith("mcp__")) {
    fields = parseMCP(toolName, obj);
  } else {
    const parser = PARSERS[toolName];
    fields = parser ? parser(obj) : [];
    if (fields.length === 0) fields = genericFields(obj);
  }
  const summary: ToolSummary = { fields, unparsed: false };
  // TodoWrite gets a rich rendering: the full task list shows below the
  // count field as a visual checklist. Both CamelCase (claude_code) and
  // snake_case (claw) tool names are accepted because both backends
  // route through this formatter.
  if (toolName === "TodoWrite" || toolName === "todo_write") {
    const todos = extractTodos(obj);
    if (todos.length > 0) summary.todos = todos;
  }
  return summary;
}
