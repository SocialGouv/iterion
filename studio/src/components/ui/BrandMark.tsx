export interface BrandMarkProps {
  className?: string;
}

/**
 * Iterion brand mark — the hexagon + fast-forward (≫) glyph that ships as
 * the favicon, redrawn as an inline SVG so it stays crisp at any size and
 * theme-stable. The hexagon is the brand indigo (`--color-accent`, identical
 * in light + dark) with white chevrons, so the mark reads the same on either
 * surface without a `dark:invert` crutch. Pair it with <BrandWordmark/>.
 */
export function BrandMark({ className = "" }: BrandMarkProps) {
  return (
    <svg
      viewBox="0 0 24 24"
      className={className}
      role="img"
      aria-label="Iterion"
      fill="none"
    >
      <polygon
        points="7,2.5 17,2.5 22.5,12 17,21.5 7,21.5 1.5,12"
        fill="var(--color-accent)"
      />
      <path
        d="M8.5 8 L13 12 L8.5 16 M12.5 8 L17 12 L12.5 16"
        stroke="var(--color-accent-fg)"
        strokeWidth="2.1"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  );
}
