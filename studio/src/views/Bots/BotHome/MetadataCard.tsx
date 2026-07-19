// Extracted from BotHome/index.tsx to keep that file focused.
// Metadata card — the existing BotMetadataForm, standalone; loose .bot
// files (no manifest.yaml) get an explainer instead.

import type { BotEntryWithSchema } from "@/api/bots";
import BotMetadataForm from "@/components/Panels/BotMetadataForm";
import { Card } from "@/components/ui";

import SectionTitle from "./SectionTitle";

export default function MetadataCard({ entry }: { entry: BotEntryWithSchema }) {
  return (
    <Card flush>
      <SectionTitle>Metadata</SectionTitle>
      {entry.is_bundle ? (
        // key re-seeds the form draft when navigating between bots.
        <BotMetadataForm key={entry.name} bot={entry} />
      ) : (
        <p className="px-4 pb-4 text-xs text-fg-muted">
          This is a loose <code>.bot</code> file — it has no manifest.yaml to edit. Package it as a
          bundle to give it a persona, description and catalog metadata.
        </p>
      )}
    </Card>
  );
}
