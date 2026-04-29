import type { IterDocument, Edge } from "@/api/types";
import type { DiagnosticIssue } from "@/api/client";
import { getAllNodeNames } from "@/lib/defaults";
import { makeEdgeId } from "@/lib/documentToGraph";

export type DiagnosticSeverity = "error" | "warning";

export interface ParsedDiagnostic {
  /** Original raw string (e.g., `error [C014]: condition field 'foo' ...`). */
  raw: string;
  /** Severity parsed from the prefix (defaults to "error" if unparseable). */
  severity: DiagnosticSeverity;
  /** Diagnostic code, e.g. "C014". Empty string if absent. */
  code: string;
  /** Message body without the "severity [code]: " prefix. */
  message: string;
}

export interface AttributedDiagnostic extends ParsedDiagnostic {
  /** Node id this diagnostic was attributed to. */
  nodeId?: string;
  /** Edge id this diagnostic was attributed to. */
  edgeId?: string;
  /** Optional fix hint (preferred over the static client-side hint table). */
  hint?: string;
  /** "structured" when sourced from server `issues`, "heuristic" otherwise. */
  source: "structured" | "heuristic";
}

export interface GroupedDiagnostics {
  byNode: Map<string, AttributedDiagnostic[]>;
  byEdge: Map<string, AttributedDiagnostic[]>;
  global: AttributedDiagnostic[];
  all: AttributedDiagnostic[];
}

const PREFIX_RE = /^(error|warning)\s*\[(C\d+)\]\s*:\s*/i;
const EDGE_RE = /\b([A-Za-z_][A-Za-z0-9_]*)\s*->\s*([A-Za-z_][A-Za-z0-9_]*)\b/;

/**
 * Parse a single diagnostic string produced by `Diagnostic.Error()` on the
 * Go side: `"<severity> [<code>]: <message>"`. Returns the unparsed input
 * gracefully when the prefix is absent.
 */
export function parseDiagnostic(
  raw: string,
  fallbackSeverity: DiagnosticSeverity = "error",
): ParsedDiagnostic {
  const match = raw.match(PREFIX_RE);
  if (!match) {
    return { raw, severity: fallbackSeverity, code: "", message: raw };
  }
  const sev = match[1]!.toLowerCase() === "warning" ? "warning" : "error";
  return {
    raw,
    severity: sev,
    code: match[2]!.toUpperCase(),
    message: raw.slice(match[0].length),
  };
}

/**
 * Best-effort attribution: find which declared node and/or edge the
 * diagnostic mentions. The attribution is a heuristic on the message text;
 * Phase 7 will replace it with authoritative IDs from the Go validator.
 */
function attribute(
  parsed: ParsedDiagnostic,
  nodeNames: string[],
  edgeIdByEndpoints: Map<string, string>,
): AttributedDiagnostic {
  const m = parsed.message;
  let edgeId: string | undefined;
  const edgeMatch = m.match(EDGE_RE);
  if (edgeMatch) {
    edgeId = edgeIdByEndpoints.get(`${edgeMatch[1]}->${edgeMatch[2]}`);
  }
  let nodeId: string | undefined;
  // Prefer the longest matching name to avoid prefix collisions.
  const candidates = [...nodeNames].sort((a, b) => b.length - a.length);
  for (const name of candidates) {
    const escaped = name.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    if (new RegExp(`\\b${escaped}\\b`).test(m)) {
      nodeId = name;
      break;
    }
  }
  return { ...parsed, nodeId, edgeId, source: "heuristic" };
}

/**
 * Build the lookup `{from}->{to}` -> first matching edge id. When two edges
 * share the same endpoints we keep the first one; the heuristic can't
 * disambiguate further from text alone (Phase 7 will).
 */
function buildEdgeLookup(doc: IterDocument | null): Map<string, string> {
  const lookup = new Map<string, string>();
  if (!doc) return lookup;
  for (const wf of doc.workflows ?? []) {
    const edges = wf.edges ?? [];
    edges.forEach((e: Edge, i: number) => {
      const key = `${e.from}->${e.to}`;
      if (!lookup.has(key)) lookup.set(key, makeEdgeId(wf.name, i));
    });
  }
  return lookup;
}

function fromIssue(issue: DiagnosticIssue): AttributedDiagnostic {
  return {
    raw: issue.message,
    severity: issue.severity,
    code: issue.code ?? "",
    message: issue.message,
    nodeId: issue.node_id || undefined,
    edgeId: issue.edge_id || undefined,
    hint: issue.hint || undefined,
    source: "structured",
  };
}

export function groupDiagnostics(
  errors: string[],
  warnings: string[],
  doc: IterDocument | null,
  issues?: DiagnosticIssue[],
): GroupedDiagnostics {
  const nodeNames: string[] = doc ? Array.from(getAllNodeNames(doc)) : [];
  const edgeLookup = buildEdgeLookup(doc);

  let all: AttributedDiagnostic[];
  if (issues && issues.length > 0) {
    // Authoritative path: server provided structured fields.
    all = issues.map(fromIssue);
  } else {
    // Fallback: parse strings and run the heuristic attribution.
    all = [
      ...errors.map((s) => attribute(parseDiagnostic(s, "error"), nodeNames, edgeLookup)),
      ...warnings.map((s) => attribute(parseDiagnostic(s, "warning"), nodeNames, edgeLookup)),
    ];
  }

  const byNode = new Map<string, AttributedDiagnostic[]>();
  const byEdge = new Map<string, AttributedDiagnostic[]>();
  const global: AttributedDiagnostic[] = [];

  for (const d of all) {
    if (d.edgeId) {
      const list = byEdge.get(d.edgeId) ?? [];
      list.push(d);
      byEdge.set(d.edgeId, list);
    } else if (d.nodeId) {
      const list = byNode.get(d.nodeId) ?? [];
      list.push(d);
      byNode.set(d.nodeId, list);
    } else {
      global.push(d);
    }
  }

  return { byNode, byEdge, global, all };
}

export function dominantSeverity(
  diags: AttributedDiagnostic[] | undefined,
): DiagnosticSeverity | null {
  if (!diags || diags.length === 0) return null;
  for (const d of diags) if (d.severity === "error") return "error";
  return "warning";
}
