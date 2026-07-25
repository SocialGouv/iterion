// Header-slot actions for the board. Schema/maintenance actions collapse
// behind one "Manage" menu so the header reads: one primary (create) + one
// secondary (manage).

import { Button } from "@/components/ui/Button";
import { DropdownMenu, DropdownMenuItem } from "@/components/ui/DropdownMenu";

export function BoardHeaderActions({
  onManageLabels,
  onManageFields,
  onAddColumn,
  onRefresh,
  onNewIssue,
}: {
  onManageLabels: () => void;
  onManageFields: () => void;
  onAddColumn: () => void;
  onRefresh: () => void;
  onNewIssue: () => void;
}) {
  return (
    <>
      <DropdownMenu
        trigger={
          <Button variant="secondary" size="sm">
            Manage
          </Button>
        }
        align="end"
      >
        <DropdownMenuItem onSelect={onManageLabels}>Labels…</DropdownMenuItem>
        <DropdownMenuItem onSelect={onManageFields}>Fields…</DropdownMenuItem>
        <DropdownMenuItem onSelect={onAddColumn}>Add column…</DropdownMenuItem>
        <DropdownMenuItem onSelect={onRefresh}>Refresh</DropdownMenuItem>
      </DropdownMenu>
      <Button variant="primary" size="sm" onClick={onNewIssue}>
        + New issue
      </Button>
    </>
  );
}
