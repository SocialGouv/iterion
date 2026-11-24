import type { ComponentType, SVGProps } from "react";

// IconComponent is any icon rendering a bare <svg> from SVG props:
// Radix-style components and the @heroicons family (whose props type
// narrows children away, hence the `children?: undefined` in the
// contract — this alias's single widening point heals every call site).
export type IconComponent = ComponentType<
  SVGProps<SVGSVGElement> & { children?: undefined }
>;

// Icon renders an icon glyph with its accessibility contract decided
// once, at this chokepoint, instead of per call-site:
//
//   - a `label` marks the icon as information-bearing: it renders
//     role="img" + aria-label (RGAA 1.1 — the accessible name and the
//     image role must sit on the same element, and a bare Radix <svg>
//     carries neither, so a label alone is dropped by assistive tech);
//   - no `label` marks it as decoration: it renders aria-hidden="true"
//     (RGAA 1.2), including inside an already-labelled button where an
//     unnamed <svg> would otherwise stay exposed.
//
// Route every labelled or decorative Radix icon through here — never
// spread a bare aria-label onto an icon again. The jsx-content guard
// (`__tests__/a11y/jsx-content.test.ts`) refuses an `*Icon` element
// that carries an aria-label without role="img", so the class cannot
// quietly come back around this primitive.
export function Icon({
  icon: Glyph,
  label,
  className,
}: {
  icon: IconComponent;
  /** Accessible name; omit for a purely decorative glyph. */
  label?: string;
  className?: string;
}) {
  return label ? (
    <Glyph role="img" aria-label={label} className={className} />
  ) : (
    <Glyph aria-hidden="true" className={className} />
  );
}
