// One catalog card: the mark, the name, one badge per fact that decides a
// choice (category, maturity, tier), and the counts that say whether it is
// already in use.
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Card } from "@/components/ui/Card";
import { ClampText } from "@/components/ui/ClampText";
import { SourceLogo } from "@/components/Sources/SourceLogo";
import { sourceCategoryLabel, type Source } from "@/api/sources";

const MATURITY_LABELS: Record<string, string> = {
  spotted: "Repéré",
  experimental: "Expérimental",
  qualified: "Qualifié",
  deprecated: "Déprécié",
};

export function SourceCard({
  source,
  onOpen,
}: {
  source: Source;
  onOpen: () => void;
}) {
  return (
    <Card interactive className="flex flex-col gap-1.5" onClick={onOpen}>
      <div className="flex items-center gap-2">
        <SourceLogo connectorId={source.id} />
        <span className="text-sm font-medium text-fg-default">
          {source.display_name}
        </span>
        {source.maturity && (
          <Badge variant={source.maturity === "qualified" ? "success" : "neutral"}>
            {MATURITY_LABELS[source.maturity] ?? source.maturity}
          </Badge>
        )}
      </div>
      <ClampText text={source.description ?? ""} lines={2} className="text-xs text-fg-subtle" />
      <div className="flex items-center gap-1.5 text-xs text-fg-subtle">
        <Badge variant="neutral">{sourceCategoryLabel(source.category)}</Badge>
        {source.tier && <Badge variant="neutral">{source.tier}</Badge>}
      </div>
      <div className="mt-auto flex items-center justify-between pt-1.5 text-xs text-fg-subtle">
        <span>
          {source.connections} connexion{source.connections > 1 ? "s" : ""}
          {" · "}
          {source.subscriptions} abonnement{source.subscriptions > 1 ? "s" : ""}
        </span>
        <Button
          variant="secondary"
          size="sm"
          onClick={onOpen}
          aria-label={`${source.connections > 0 ? "Gérer" : "Brancher"} ${source.display_name}`}
        >
          {source.connections > 0 ? "Gérer" : "Brancher"}
        </Button>
      </div>
    </Card>
  );
}
