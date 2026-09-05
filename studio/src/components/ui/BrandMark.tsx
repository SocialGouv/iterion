import iterionMark from "@/assets/iterion-mark.png";

export interface BrandMarkProps {
  className?: string;
}

/**
 * Iterion brand mark — the mascot of the official iterion-bot account in its
 * badge form (dark disc + ring), rendered at 256 px from the brand master by
 * `task brand:gen` so it stays crisp at 4× DPR in the 28–64 px slots it fills.
 * A raster on purpose: the mascot is an illustration, not a glyph. The disc
 * carries its own background, so it reads the same on either theme. Pair it
 * with <BrandWordmark/>.
 */
export function BrandMark({ className = "" }: BrandMarkProps) {
  return (
    <img
      src={iterionMark}
      alt="Iterion"
      draggable={false}
      className={`select-none ${className}`.trim()}
    />
  );
}
