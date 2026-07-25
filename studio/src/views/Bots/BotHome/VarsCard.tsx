// Extracted from BotHome/index.tsx to keep that file focused.
// Variables card — the vars the workflow declares, as a read-only table.

import type { BotEntryWithSchema } from "@/api/bots";
import { literalToString } from "@/components/Runs/launchView/utils";
import { Card, Table, TBody, Td, Th, THead, Tr } from "@/components/ui";

import SectionTitle from "./SectionTitle";

export default function VarsCard({ entry }: { entry: BotEntryWithSchema }) {
  const fields = entry.vars?.fields ?? [];
  return (
    <Card flush>
      <SectionTitle>Variables</SectionTitle>
      {fields.length === 0 ? (
        <p className="px-4 pb-4 text-xs text-fg-subtle">This workflow declares no vars.</p>
      ) : (
        <div className="overflow-x-auto pb-1">
          <Table caption={`Vars declared by ${entry.name}`}>
            <THead>
              <Th>Name</Th>
              <Th>Type</Th>
              <Th>Default</Th>
            </THead>
            <TBody>
              {fields.map((f) => (
                <Tr key={f.name}>
                  <Td className="font-mono text-fg-default">{f.name}</Td>
                  <Td className="text-fg-muted">{f.type}</Td>
                  <Td className="font-mono text-fg-muted">
                    {f.default ? literalToString(f.default) || <Em>empty</Em> : <Em>required</Em>}
                  </Td>
                </Tr>
              ))}
            </TBody>
          </Table>
        </div>
      )}
    </Card>
  );
}

function Em({ children }: { children: React.ReactNode }) {
  return <span className="font-sans italic text-fg-subtle">{children}</span>;
}
