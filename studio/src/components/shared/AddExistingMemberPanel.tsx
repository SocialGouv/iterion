// AddExistingMemberPanel — "this person already has an account; put them
// here with this role".
//
// It is the counterpart of the email invitation, for the case the
// invitation exists to solve and cannot. One component rather than one
// panel per roster: the team page and the org page ask the same question,
// and two copies would drift the moment one of them grew a guard. (The
// super-admin user drawer asks the INVERSE question — "which org or team
// for this account" — so it picks a scope, not a person, and correctly
// does not use this.)
//
// Two candidate sources, one shape. Pass `onQueryChange` when the parent
// resolves candidates from the server (the super-admin user search, whose
// match is an email PREFIX); omit it to filter a list the parent already
// holds (a team's candidates are its org's roster). Either way the search
// box is the Combobox's own — a second input above it would read as a
// rival way to do the same thing.

import { useState } from "react";

import { Button } from "@/components/ui/Button";
import { Card } from "@/components/ui/Card";
import { Combobox, type ComboboxOption } from "@/components/ui/Combobox";
import { RoleSelect } from "@/components/shared/RoleSelect";
import type { Confirmer } from "@/hooks/useConfirm";
import { confirmOwnerGrant } from "@/lib/roles";

export interface MemberCandidate {
  user_id: string;
  email?: string;
  name?: string;
}

export function AddExistingMemberPanel({
  title,
  description,
  candidates,
  loading = false,
  busy = false,
  roles,
  defaultRole,
  emptyMessage,
  addLabel = "Add",
  onQueryChange,
  queryPlaceholder = "Search by email…",
  confirm,
  onAdd,
}: {
  title: string;
  description?: React.ReactNode;
  candidates: MemberCandidate[];
  loading?: boolean;
  busy?: boolean;
  roles: readonly string[];
  defaultRole: string;
  emptyMessage: string;
  addLabel?: string;
  onQueryChange?: (query: string) => void;
  queryPlaceholder?: string;
  /** Prompts before an `owner` grant — control is handed over either way. */
  confirm?: Confirmer;
  onAdd: (userID: string, role: string) => Promise<void>;
}) {
  const [userID, setUserID] = useState("");
  const [role, setRole] = useState(defaultRole);
  // The picked account, kept so it survives its own selection. Committing a
  // pick closes the Combobox, which clears the search — and with a
  // server-backed `candidates` that empties the list the pick came from. The
  // operator would then face a panel that names nobody while "Add" stays
  // enabled, one click from placing an account they can no longer see.
  const [picked, setPicked] = useState<MemberCandidate | null>(null);

  // The pin survives the search being CLEARED — the case it exists for,
  // since committing a pick empties a server-backed list — but not the
  // roster LOSING the account. A non-empty list is the signal the roster has
  // spoken; if the pick is absent from it, the account went away between the
  // pick and the click, and the selection is reconciled to nothing rather
  // than left armed on an id the write would 404 on.
  const rosterSpoke = candidates.length > 0;
  const stale = picked != null && rosterSpoke && !candidates.some((c) => c.user_id === picked.user_id);
  const shown = picked && !rosterSpoke ? [picked, ...candidates] : candidates;
  // Derived, not held: the same reconciliation the drawer applies to its org
  // pick. A stale selection disarms the button as well as leaving the list.
  const effectiveUserID = stale ? "" : userID;

  const options: ComboboxOption<string>[] = shown.map((c) => ({
    value: c.user_id,
    label: c.email ?? c.user_id,
    description: c.name,
    // The id is searchable but never the primary line: an operator reads
    // addresses, and pastes ids.
    searchHaystack: `${c.email ?? ""} ${c.name ?? ""} ${c.user_id}`,
  }));

  const submit = async () => {
    if (!effectiveUserID) return;
    // The prompt belongs to the WRITE, not to the dropdown. Guarding the
    // select guarded a gesture that writes nothing — and since the role is
    // deliberately kept after a successful add (adding a batch of people is
    // the actual gesture), the SECOND owner grant fired no prompt at all.
    if (!(await confirmOwnerGrant(confirm, role))) return;
    await onAdd(effectiveUserID, role);
    // Clear the selection, keep the role: adding a batch of people to one
    // team is the actual gesture, and re-picking the role each time is
    // where an operator mis-clicks.
    setUserID("");
    setPicked(null);
  };

  return (
    <Card className="space-y-3">
      <h3 className="font-medium">{title}</h3>
      {description && <p className="text-caption text-fg-subtle">{description}</p>}

      <div className="flex gap-2 items-end">
        <div className="flex-1 min-w-0">
          <label htmlFor="add-member-user" className="sr-only">
            Account
          </label>
          <Combobox
            id="add-member-user"
            size="md"
            value={effectiveUserID}
            options={options}
            placeholder={loading ? "Loading accounts…" : queryPlaceholder}
            disabled={busy}
            onQueryChange={onQueryChange}
            onChange={(v) => {
              setUserID(v);
              setPicked(shown.find((c) => c.user_id === v) ?? null);
            }}
          />
        </div>
        <div>
          <label htmlFor="add-member-role" className="sr-only">
            Role
          </label>
          <RoleSelect
            size="md"
            id="add-member-role"
            value={role}
            roles={roles}
            disabled={busy}
            onChange={setRole}
          />
        </div>
        <Button
          variant="primary"
          loading={busy}
          disabled={!effectiveUserID || busy}
          onClick={() => void submit()}
        >
          {addLabel}
        </Button>
      </div>

      {!loading && options.length === 0 && (
        <p className="text-caption text-fg-subtle">{emptyMessage}</p>
      )}
    </Card>
  );
}
