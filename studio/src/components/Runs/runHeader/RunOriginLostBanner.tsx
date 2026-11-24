// The run view's state once the server closed its socket scope_lost: the
// run this view was opened on was replaced or deleted under its ID, or the
// caller's access to it changed. The hook no longer reconnects — resuming
// would attach this run's cursors to whatever run holds the ID now — so the
// banner says the data is the original run's and offers a full reload,
// which starts every view of the run over from the server.
//
// End-user text is French; the page itself is English, so the banner names
// its language (RGAA 8.7).

import { Button, InlineBanner } from "@/components/ui";
import type { WsState } from "@/store/run";

function reloadPage(): void {
  window.location.reload();
}

export default function RunOriginLostBanner({
  state,
  onReload = reloadPage,
}: {
  state: WsState;
  onReload?: () => void;
}) {
  if (state !== "lost") return null;
  return (
    <div lang="fr">
      <InlineBanner
        tone="danger"
        title="Ce run a été remplacé ou supprimé, ou votre accès a changé."
        action={
          <Button variant="ghost" size="sm" onClick={onReload}>
            Recharger
          </Button>
        }
      >
        Les mises à jour en direct sont arrêtées : les données affichées
        sont celles du run d’origine. Rechargez pour afficher l’état actuel.
      </InlineBanner>
    </div>
  );
}
