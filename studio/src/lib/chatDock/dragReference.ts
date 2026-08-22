// Dragging an iterion element onto the assistant's composer.
//
// The EXPLICIT half of the context protocol; `routeReference.ts` is the
// implicit half ("you are looking at it"). Both mint through
// `mintReference`, so both produce the same `<kind>/<id>` wire form and
// inherit the same delimiter sanitisation — which is the point of having one
// vocabulary rather than two.
//
// Why references and not inlined content: an inlined run log freezes a
// snapshot and eats the context window; a reference lets the assistant fetch
// exactly what it needs, when it needs it, and re-read it after the run has
// moved on.
//
// Sources opt in through `referenceDragProps`. That is deliberate — issue
// #333's acceptance asks for "a shared helper, not per-view bespoke
// handlers", and a per-view handler is how half the surfaces end up wired
// and nobody remembers which.

import type { DragEvent } from "react";

import {
  mintReference,
  type ReferenceKind,
  type TypedReference,
} from "./routeReference";

// A dedicated MIME type so a drop onto the composer is unambiguous, and so a
// drop of anything ELSE (a file, a board card onto its own column) is not
// mistaken for one. The `+json` suffix follows RFC 6839.
export const REFERENCE_MIME = "application/x-iterion-reference+json";

// Cap what we will parse off a drag payload. The value is a pointer, never
// content, so anything large is either a bug or an attempt.
const MAX_PAYLOAD_BYTES = 4096;

// Maximum references attached to one message. Past a handful the operator
// cannot see what they attached, and the assistant is being handed a research
// project rather than a question.
export const MAX_ATTACHED_REFERENCES = 8;

interface ReferencePayload {
  kind: string;
  id: string;
  label?: string;
}

/** Props a draggable source spreads onto its element.
 *
 *  Usage: `<div {...referenceDragProps("run", run.id, run.name)}>`. The
 *  source does not need to know the chat exists — it publishes a typed
 *  reference and any drop target that understands the MIME type can take it.
 */
export function referenceDragProps(
  kind: ReferenceKind,
  id: string,
  label?: string,
): {
  draggable: true;
  onDragStart: (e: DragEvent) => void;
} {
  return {
    draggable: true,
    onDragStart: (e: DragEvent) => {
      const payload: ReferencePayload = { kind, id, label: label ?? id };
      e.dataTransfer.setData(REFERENCE_MIME, JSON.stringify(payload));
      // A plain-text mirror so dropping onto an ordinary text field (or
      // another app) yields something meaningful instead of nothing. It is
      // NOT what the composer reads — the typed payload above is.
      e.dataTransfer.setData("text/plain", `${kind}/${id}`);
      e.dataTransfer.effectAllowed = "copy";
      // Sources that also drag for their own purposes (a board card onto a
      // column) set their payload first and call this after; stopping
      // propagation here would break them, so we deliberately do not.
    },
  };
}

/** Add a reference to a drag a source is ALREADY writing.
 *
 *  The board and the pipeline board drag cards between columns and own their
 *  own payload; a reference rides alongside it under a different MIME type,
 *  so one gesture serves both targets and neither source needs to know about
 *  the other's.
 *
 *  It widens `effectAllowed` to include copy when the source asked for
 *  `move` only — otherwise the browser refuses the composer's copy drop and
 *  shows the "no" cursor, which reads as "this feature does not work".
 */
export function addReferenceToDrag(
  dt: DataTransfer,
  kind: ReferenceKind,
  id: string,
  label?: string,
): void {
  const payload: ReferencePayload = { kind, id, label: label ?? id };
  dt.setData(REFERENCE_MIME, JSON.stringify(payload));
  if (dt.effectAllowed === "move") dt.effectAllowed = "copyMove";
}

/** The drop effect a target should announce for this drag — "copy" when the
 *  source permits it, "move" otherwise. Announcing an effect the source
 *  disallowed makes the browser reject the drop. */
export function referenceDropEffect(dt: DataTransfer): "copy" | "move" {
  const allowed = dt.effectAllowed;
  if (allowed === "move" || allowed === "linkMove") return "move";
  return "copy";
}

/** True when a drag carries a typed iterion reference. Used by a drop target
 *  to decide whether to show its affordance at all — dragging a file over the
 *  composer must not look like it will attach a reference. */
export function hasReferenceDrag(dt: DataTransfer | null): boolean {
  if (!dt) return false;
  // `types` is the only thing readable during dragover: getData returns ""
  // until the drop event in every browser, for the obvious privacy reason.
  return Array.from(dt.types).includes(REFERENCE_MIME);
}

/** Read a dropped reference, or null when the payload is absent, malformed,
 *  oversized, or names a kind/id shape the vocabulary does not accept.
 *
 *  Null is a NO-OP for the caller, never an error toast: a drop that did not
 *  carry a reference is the common case (a file, a stray selection), not a
 *  failure the operator needs told about. */
export function readReferenceDrop(dt: DataTransfer | null): TypedReference | null {
  if (!dt) return null;
  const raw = dt.getData(REFERENCE_MIME);
  if (!raw || raw.length > MAX_PAYLOAD_BYTES) return null;

  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return null;
  }
  if (!parsed || typeof parsed !== "object") return null;

  const { kind, id, label } = parsed as ReferencePayload;
  if (typeof kind !== "string" || typeof id !== "string") return null;
  if (!isReferenceKind(kind)) return null;
  if (label !== undefined && typeof label !== "string") return null;

  // mintReference applies the shape allowlist and the delimiter strip. A
  // payload that fails either is dropped rather than repaired: repairing a
  // pointer produces one that resolves to something the operator did not
  // point at.
  return mintReference(kind, id, label ?? id);
}

// The vocabulary minus `view`: "you are on a screen" is derived from the
// route, never dragged, and accepting it here would let a source claim the
// implicit slot.
const DRAGGABLE_KINDS: ReadonlySet<string> = new Set<ReferenceKind>([
  "run",
  "node",
  "card",
  "bot",
  "repo",
]);

function isReferenceKind(value: string): value is ReferenceKind {
  return DRAGGABLE_KINDS.has(value);
}

/** Add a reference to the attached list: de-duplicated by wire form, capped,
 *  and order-preserving so the chips read in the order they were dropped. */
export function attachReference(
  current: readonly TypedReference[],
  next: TypedReference,
): TypedReference[] {
  if (current.some((r) => r.ref === next.ref)) return [...current];
  if (current.length >= MAX_ATTACHED_REFERENCES) return [...current];
  return [...current, next];
}

/** Remove one attached reference by wire form. */
export function detachReference(
  current: readonly TypedReference[],
  ref: string,
): TypedReference[] {
  return current.filter((r) => r.ref !== ref);
}
