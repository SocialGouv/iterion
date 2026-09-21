// AddExistingMemberPanel — "this person already has an account; put them
// here with this role".
//
// It is the counterpart of the email invitation, for the case the
// invitation exists to solve and cannot. One component rather than one
// panel per roster: the team page, the org page and the super-admin user
// drawer ask the same question, and three copies would drift the moment
// one of them grew a guard.
//
// Two candidate sources, one shape. Pass `onQueryChange` when the parent
// resolves candidates from the server (the super-admin user search, whose
// match is an email PREFIX); omit it to filter a list the parent already
// holds (a team's candidates are its org's roster).

import { useMemo, useState } from "react";

import { Button } from "@/components/ui/Button";
import { Combobox, type ComboboxOption } from "@/components/ui/Combobox";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";

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
  roleLabel = (r) => r,
  emptyMessage,
  addLabel = "Add",
  onQueryChange,
  queryPlaceholder = "Search by email…",
  onAdd,
}: {
  title: string;
  description?: React.ReactNode;
  candidates: MemberCandidate[];
  loading?: boolean;
  busy?: boolean;
  roles: readonly string[];
  defaultRole: string;
  roleLabel?: (role: string) => string;
  emptyMessage: string;
  addLabel?: string;
  onQueryChange?: (query: string) => void;
  queryPlaceholder?: string;
  onAdd: (userID: string, role: string) => Promise<void>;
}) {
  const [userID, setUserID] = useState("");
  const [role, setRole] = useState(defaultRole);
  const [query, setQuery] = useState("");

  const options = useMemo<ComboboxOption<string>[]>(
    () =>
      candidates.map((c) => ({
        value: c.user_id,
        label: c.email ?? c.user_id,
        description: c.name,
        // The id is searchable but never the primary line: an operator
        // reads addresses, and pastes ids.
        searchHaystack: `${c.email ?? ""} ${c.name ?? ""} ${c.user_id}`,
      })),
    [candidates],
  );

  const serverSearched = onQueryChange != null;
  const submit = async () => {
    if (!userID) return;
    await onAdd(userID, role);
    // Clear the selection, keep the role: adding a batch of people to one
    // team is the actual gesture, and re-picking the role each time is
    // where an operator mis-clicks.
    setUserID("");
  };

  return (
    <section className="bg-surface-1 border border-border-subtle rounded-[var(--radius-lg)] shadow-[var(--shadow-sm)] p-4 space-y-3">
      <h3 className="font-medium">{title}</h3>
      {description && <p className="text-caption text-fg-subtle">{description}</p>}

      {serverSearched && (
        <div>
          <label htmlFor="add-member-query" className="sr-only">
            Search accounts
          </label>
          <Input
            size="md"
            id="add-member-query"
            placeholder={queryPlaceholder}
            value={query}
            onChange={(e) => {
              setQuery(e.target.value);
              // A new search invalidates the pick: committing the previous
              // selection against a list the operator can no longer see is
              // how the wrong account gets added.
              setUserID("");
              onQueryChange(e.target.value);
            }}
          />
        </div>
      )}

      <div className="flex gap-2 items-end">
        <div className="flex-1 min-w-0">
          <label htmlFor="add-member-user" className="sr-only">
            Account
          </label>
          <Combobox
            id="add-member-user"
            size="md"
            value={userID}
            options={options}
            placeholder={
              loading
                ? "Loading accounts…"
                : options.length === 0
                  ? emptyMessage
                  : "Pick an account…"
            }
            disabled={busy || loading || options.length === 0}
            onChange={(v) => setUserID(v)}
          />
        </div>
        <div>
          <label htmlFor="add-member-role" className="sr-only">
            Role
          </label>
          <Select
            size="md"
            id="add-member-role"
            value={role}
            disabled={busy}
            onChange={(e) => setRole(e.target.value)}
          >
            {roles.map((r) => (
              <option key={r} value={r}>
                {roleLabel(r)}
              </option>
            ))}
          </Select>
        </div>
        <Button
          variant="primary"
          loading={busy}
          disabled={!userID || busy}
          onClick={() => void submit()}
        >
          {addLabel}
        </Button>
      </div>

      {!loading && options.length === 0 && (
        <p className="text-caption text-fg-subtle">{emptyMessage}</p>
      )}
    </section>
  );
}
