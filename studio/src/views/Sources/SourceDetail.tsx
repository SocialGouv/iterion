// The catalog drawer: one source's declared triggers with the values a
// delivery carries and the narrowings an operator may pick, its auth
// schemes, and the entry into the plug wizard. Read-only — the wizard
// decides.
import { useLocation } from "wouter";

import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Drawer } from "@/components/ui/Drawer";
import { SourceLogo } from "@/components/Sources/SourceLogo";
import {
  pollContractText,
  sourceCategoryLabel,
  sourceDeliveryLabel,
  type Source,
} from "@/api/sources";

const MATURITY_LABELS: Record<string, string> = {
  spotted: "Repéré",
  experimental: "Expérimental",
  qualified: "Qualifié",
  deprecated: "Déprécié",
};

export function SourceDetail({
  source,
  onClose,
}: {
  source: Source | null;
  onClose: () => void;
}) {
  // The router's navigation stays under the studio's base (/studio in the
  // cloud); a bare window.location write would leave it (scope.ts).
  const [, navigate] = useLocation();
  return (
    <Drawer
      open={source !== null}
      onOpenChange={(o) => {
        if (!o) onClose();
      }}
      title={
        source ? (
          <span className="flex items-center gap-2">
            <SourceLogo connectorId={source.id} />
            {source.display_name}
          </span>
        ) : null
      }
      description={source?.description}
      footer={
        source?.kind === "connector" ? (
          <Button
            variant="primary"
            onClick={() => navigate(`/sources/plug?source=${encodeURIComponent(source.id)}`)}
          >
            Brancher sur un bot
          </Button>
        ) : null
      }
    >
      {source && (
        <div className="flex flex-col gap-4 p-4 text-sm text-fg-default">
          <div className="flex flex-wrap gap-1.5">
            {source.category && (
              <Badge variant="neutral">{sourceCategoryLabel(source.category)}</Badge>
            )}
            {source.maturity && (
              <Badge variant={source.maturity === "qualified" ? "success" : "neutral"}>
                {MATURITY_LABELS[source.maturity] ?? source.maturity}
              </Badge>
            )}
            {source.tier && <Badge variant="neutral">catalogue {source.tier}</Badge>}
            {source.delivery && (
              <Badge variant={source.delivery === "push" ? "neutral" : "info"}>
                {sourceDeliveryLabel(source.delivery)}
              </Badge>
            )}
          </div>

          <div className="text-xs text-fg-subtle">
            {source.connections} connexion{source.connections > 1 ? "s" : ""} ·{" "}
            {source.subscriptions} abonnement{source.subscriptions > 1 ? "s" : ""}
          </div>

          {source.auth && source.auth.length > 0 && (
            <section>
              <h3 className="text-xs font-semibold uppercase tracking-wide text-fg-subtle">
                Authentification
              </h3>
              <ul className="mt-1 flex flex-col gap-1">
                {source.auth.map((a) => (
                  <li key={a.id} className="text-xs text-fg-default">
                    <code>{a.id}</code> — {a.display_name || a.kind}
                  </li>
                ))}
              </ul>
            </section>
          )}

          {source.triggers && source.triggers.length > 0 && (
            <section>
              <h3 className="text-xs font-semibold uppercase tracking-wide text-fg-subtle">
                Événements
              </h3>
              <ul className="mt-1 flex flex-col gap-2">
                {source.triggers.map((t) => (
                  <li key={t.id} className="rounded-[var(--radius-md)] border border-border-default p-2">
                    <div className="flex items-center gap-1.5 text-xs font-medium text-fg-default">
                      {t.display_name || t.id}
                      {source.delivery === "mixed" && t.delivery && (
                        <Badge variant={t.delivery === "poll" ? "info" : "neutral"}>
                          {sourceDeliveryLabel(t.delivery)}
                        </Badge>
                      )}
                    </div>
                    {t.description && (
                      <div className="text-xs text-fg-subtle mt-0.5">{t.description}</div>
                    )}
                    {t.delivery === "poll" && (
                      <div className="mt-1 text-xs text-fg-subtle">{pollContractText(t.interval)}</div>
                    )}
                    {t.query && t.query.length > 0 && (
                      <div className="mt-1 text-xs text-fg-subtle">
                        Requête :{" "}
                        {t.query.map((q) => (
                          <code key={q.id} className="mr-1">
                            {q.id}
                            {q.required ? " *" : ""}
                          </code>
                        ))}
                      </div>
                    )}
                    {t.outputs && t.outputs.length > 0 && (
                      <div className="mt-1 text-xs text-fg-subtle">
                        Valeurs :{" "}
                        {t.outputs.map((o) => (
                          <code key={o.name} className="mr-1">
                            {o.name}
                          </code>
                        ))}
                      </div>
                    )}
                    {t.filters && t.filters.length > 0 && (
                      <div className="mt-1 text-xs text-fg-subtle">
                        Filtres :{" "}
                        {t.filters.map((f) => (
                          <code key={f.id} className="mr-1">
                            {f.id}
                          </code>
                        ))}
                      </div>
                    )}
                  </li>
                ))}
              </ul>
            </section>
          )}
        </div>
      )}
    </Drawer>
  );
}
