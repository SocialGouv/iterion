import { useState, type ReactNode } from "react";

/**
 * The drop zone whose LABEL owns the file input: one named control opens the
 * picker, by pointer and by key, and the box around it carries the drag
 * handlers and the styling without claiming to be a control.
 *
 * Why it is a component and not a props helper (#2766): the zone used to
 * spread `clickableRowProps` — `role="button"` + tabindex — around a
 * focusable `sr-only` file input, which is a button containing a control
 * (axe `nested-interactive`, WCAG 4.1.2), announced twice under the same
 * name, with the row's Enter and the input's own activation both reaching
 * the picker. The web's answer is a `<label>`: it needs no role, no
 * tabindex and no key handler, because the input it owns already has all
 * three.
 *
 * The input stays IN the accessibility tree (`sr-only`, named) because the
 * operator reaches and operates it — the opposite case, an input a visible
 * button drives and nobody else reaches, is `HiddenFileInput`.
 *
 * `disabled` and `busy` are both inert, and deliberately not the same
 * state: `disabled` dims the zone (`data-disabled`, so the dimming follows
 * the attribute and never a static opacity — RGAA 3.2), while `busy` keeps
 * its contrast because the text it shows while in flight has to stay
 * readable. Each one disables the INPUT, so the pointer and the keyboard
 * refuse together — a zone made inert by `pointer-events-none` alone stays
 * operable by key, and one made inert by key alone stays clickable.
 */
export function FileDropZone({
  name,
  accept,
  onPick,
  disabled,
  busy,
  invalid,
  children,
}: {
  /** The picker's accessible name — the ONE name this zone announces. */
  name: string;
  /** `accept` attribute value; undefined accepts anything. */
  accept?: string;
  /** The chosen file, or null when the operator cleared the picker. */
  onPick: (file: File | null) => void;
  /** Inert AND dimmed: the operator must see that it is out of service. */
  disabled?: boolean;
  /** Inert while an upload is in flight; the zone keeps its contrast. */
  busy?: boolean;
  /** The last file was refused — carried by the input, which holds it. */
  invalid?: boolean;
  children: ReactNode;
}) {
  const [dragging, setDragging] = useState(false);
  const inert = Boolean(disabled || busy);

  return (
    <div
      aria-busy={busy || undefined}
      data-disabled={disabled || undefined}
      className={[
        "rounded-md border-dashed border p-3 text-center transition-colors data-[disabled]:opacity-60",
        // The focus indicator of an sr-only input: focus lands on a control
        // no pixel shows, so the box it belongs to shows it (RGAA 10.7).
        "focus-within:border-accent focus-within:ring-1 focus-within:ring-accent",
        dragging ? "border-accent bg-accent-soft" : "border-border-default",
        invalid ? "border-danger ring-1 ring-danger" : "",
        inert ? "pointer-events-none" : "hover:border-accent",
      ]
        .filter(Boolean)
        .join(" ")}
      onDragOver={(e) => {
        if (inert) return;
        e.preventDefault();
        setDragging(true);
      }}
      onDragLeave={() => setDragging(false)}
      onDrop={(e) => {
        e.preventDefault();
        setDragging(false);
        // The guard is here and not in the CSS: `pointer-events-none` is a
        // stylesheet's refusal, and this one has to hold wherever the drop
        // arrives from.
        if (inert) return;
        const file = e.dataTransfer.files[0];
        if (file) onPick(file);
      }}
    >
      <label className="flex flex-col items-center justify-center gap-1.5 cursor-pointer">
        {children}
        <input
          type="file"
          accept={accept}
          className="sr-only"
          aria-label={name}
          aria-invalid={invalid || undefined}
          disabled={inert}
          onChange={(e) => {
            const file = e.target.files?.[0] ?? null;
            // Reset so re-picking the SAME file fires onChange again — the
            // operator replacing a file they just removed.
            e.target.value = "";
            onPick(file);
          }}
        />
      </label>
    </div>
  );
}
