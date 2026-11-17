// The launch form's codemode override (CM-5f, ADR-165): the mode select,
// and behind a disclosure the seven bounds an operator can raise for this
// run — the HTTP twin of the CLI's --codemode / --codemode-* flags.
// Presentational: useRunOverrides owns the state and folds it into
// createRun() via buildCodemodePayload.
//
// Placeholders come from `codemode_limits.default` (served by the preview
// from scripthost) and the over-ceiling warning from `.ceiling` — the form
// WARNS and never blocks: the clamp runs at spawn and reports through
// OnClamp, so refusing here would give the bound a second owner.

import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import type { CodemodeLimits } from "@/api/runs";

import {
  boundsSet,
  overCeiling,
  type CodemodeFieldValues,
} from "./codemodePayload";

export interface CodemodeSectionProps {
  mode: string;
  values: CodemodeFieldValues;
  show: boolean;
  limits: CodemodeLimits | undefined;
  /** Per-node route refusals from the preview (#3259), keyed by node id:
   *  a node that will run with codemode on a backend that serves none. */
  routeRefusals: Record<string, string>;
  onModeChange: (value: string) => void;
  onToggle: () => void;
  onChange: (patch: Partial<CodemodeFieldValues>) => void;
}

interface RowSpec {
  key: keyof CodemodeFieldValues;
  id: string;
  label: string;
  sub: string;
  type: "number" | "text";
  step?: string;
}

const rows: RowSpec[] = [
  { key: "toolCalls", id: "launch-codemode-tool-calls", label: "tool_calls", sub: "calls per program", type: "number", step: "1" },
  { key: "wall", id: "launch-codemode-wall", label: "wall", sub: "wall-clock bound", type: "text" },
  { key: "cpu", id: "launch-codemode-cpu", label: "cpu", sub: "CPU-seconds bound", type: "text" },
  { key: "memory", id: "launch-codemode-memory", label: "memory", sub: "bytes", type: "number", step: "1048576" },
  { key: "scriptBytes", id: "launch-codemode-script-bytes", label: "script_bytes", sub: "program size", type: "number", step: "1024" },
  { key: "resultBytes", id: "launch-codemode-result-bytes", label: "result_bytes", sub: "returned value", type: "number", step: "1024" },
  { key: "toolResultBytes", id: "launch-codemode-tool-result-bytes", label: "tool_result_bytes", sub: "one tool result", type: "number", step: "1024" },
];

function placeholderOf(
  limits: CodemodeLimits | undefined,
  key: keyof CodemodeFieldValues,
): string {
  const def = limits?.default;
  if (!def) return "inherits";
  switch (key) {
    case "toolCalls":
      return def.tool_calls !== undefined ? String(def.tool_calls) : "inherits";
    case "wall":
      return def.wall || "inherits";
    case "cpu":
      return def.cpu || "inherits";
    case "memory":
      return def.memory !== undefined ? String(def.memory) : "inherits";
    case "scriptBytes":
      return def.script_bytes !== undefined ? String(def.script_bytes) : "inherits";
    case "resultBytes":
      return def.result_bytes !== undefined ? String(def.result_bytes) : "inherits";
    case "toolResultBytes":
      return def.tool_result_bytes !== undefined
        ? String(def.tool_result_bytes)
        : "inherits";
  }
}

export default function CodemodeSection({
  mode,
  values,
  show,
  limits,
  routeRefusals,
  onModeChange,
  onToggle,
  onChange,
}: CodemodeSectionProps) {
  const over = overCeiling(values, limits);
  const refusedNodes = Object.keys(routeRefusals);
  return (
    <div className="mt-6 border-t border-border-default pt-4">
      <div className="grid grid-cols-[160px_1fr] gap-3 items-start">
        <label htmlFor="launch-codemode-mode" className="pt-1">
          <div className="text-xs font-medium font-mono">codemode</div>
          <div className="text-caption text-fg-subtle">programs as tool calls</div>
        </label>
        <div>
          <Select
            id="launch-codemode-mode"
            value={mode}
            onChange={(e) => onModeChange(e.currentTarget.value)}
          >
            <option value="">default — off unless the bot asks for it</option>
            <option value="on">on — hand agent/judge nodes the codemode tool</option>
            <option value="off">off — keep it away for this run</option>
          </Select>
          <div className="mt-1 text-caption text-fg-subtle">
            One extra tool whose input is a <em>program</em>, run in a
            child process that holds no credential and can only call the
            node&apos;s own tools — the model loops, aggregates and filters
            out of its context (ADR-165). Served by <code>claw</code> today;
            a node routed elsewhere is refused at dispatch, and the picker
            above names it. Off is the default: what a model is handed is
            opted into.
          </div>
          {refusedNodes.length > 0 && (
            <div className="mt-1 text-caption text-warning">
              {refusedNodes.length === 1
                ? `node ${refusedNodes[0]} will be refused at dispatch — see its backend picker`
                : `${refusedNodes.length} nodes will be refused at dispatch — see their backend pickers`}
            </div>
          )}
          <div className="mt-2">
            <button
              type="button"
              className="text-xs text-fg-muted hover:text-fg-default flex items-center gap-1"
              onClick={onToggle}
              title="Raise a codemode bound for this run without editing the bot."
            >
              <span>{show ? "▾" : "▸"}</span>
              <span>
                Bounds{boundsSet(values) ? " — overriding the bot" : ""}
              </span>
            </button>
          </div>
        </div>
      </div>
      {show && (
        <div className="mt-3 space-y-3 pl-4 border-l border-border-default">
          <div className="text-caption text-fg-subtle">
            Non-empty fields override the bot&apos;s <code>codemode:</code>{" "}
            bounds for this run only — same precedence as the CLI{" "}
            <code>--codemode-*</code> flags. Empty = inherits. Durations are Go
            durations (<code>30s</code>, <code>5m</code>); sizes and counts are
            raw integers. Anything above the platform ceiling still runs at the
            ceiling, and the run log says so.
          </div>
          {rows.map((row) => (
            <div
              key={row.key}
              className="grid grid-cols-[160px_1fr] gap-3 items-start"
            >
              <label htmlFor={row.id} className="pt-1">
                <div className="text-xs font-medium font-mono">{row.label}</div>
                <div className="text-caption text-fg-subtle">{row.sub}</div>
              </label>
              <div>
                <Input
                  id={row.id}
                  size="sm"
                  type={row.type}
                  min={row.type === "number" ? 0 : undefined}
                  step={row.step}
                  className="font-mono"
                  placeholder={placeholderOf(limits, row.key)}
                  value={values[row.key]}
                  onChange={(e) => onChange({ [row.key]: e.target.value })}
                />
              </div>
            </div>
          ))}
          {over.length > 0 && (
            <div role="status" className="text-caption text-warning">
              above the platform ceiling: <code>{over.join(", ")}</code> — the
              run still starts, at the ceiling (the clamp report names it).
            </div>
          )}
        </div>
      )}
    </div>
  );
}
