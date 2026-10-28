import type { ChangeEvent, RefObject } from "react";

/**
 * The file input a BUTTON drives: the operator presses the visible button,
 * which calls `inputRef.current.click()`, and never reaches the input itself.
 * It is therefore kept OUT of the accessibility tree — `aria-hidden` plus
 * `tabIndex={-1}` — and the button beside it carries the name.
 *
 * The two neighbouring shapes, and why neither belongs here:
 *
 *  - `className="hidden"` and nothing else: `display:none` hides it from
 *    assistive tech only while that class applies, so every net that reads
 *    the DOM (jsdom loads no stylesheet) sees a focusable file picker with no
 *    accessible name — axe `label`, and a source scan that can only stay
 *    quiet by exempting the whole `hidden` utility.
 *  - `sr-only` with an `aria-label`, for an input the operator DOES reach and
 *    operate (GateFileInput, AttachmentFieldInput): it must stay focusable
 *    and announce itself, so it must NOT use this component.
 *
 * One component rather than the attribute pair repeated per call site, so the
 * pair cannot drift between them.
 */
export function HiddenFileInput({
  inputRef,
  accept,
  onChange,
}: {
  /** The ref the driving button clicks. */
  inputRef: RefObject<HTMLInputElement | null>;
  /** `accept` attribute value, e.g. ".botz". */
  accept: string;
  onChange: (event: ChangeEvent<HTMLInputElement>) => void;
}) {
  return (
    <input
      ref={inputRef}
      type="file"
      accept={accept}
      className="hidden"
      aria-hidden="true"
      tabIndex={-1}
      onChange={onChange}
    />
  );
}
