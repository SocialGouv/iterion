// An <img> against the icon route (GET /api/v1/connectors/{id}/icon): the
// server answers with the sanitized mark or the contrast-checked monogram,
// always an image, so the component carries no fallback of its own — a
// failed load is a broken tile like any other image.
import { sourceIconURL } from "@/api/sources";

export function SourceLogo({
  connectorId,
  className = "h-8 w-8 rounded-[var(--radius-md)]",
}: {
  connectorId: string;
  className?: string;
}) {
  return (
    <img
      src={sourceIconURL(connectorId)}
      alt=""
      className={className}
      loading="lazy"
    />
  );
}
