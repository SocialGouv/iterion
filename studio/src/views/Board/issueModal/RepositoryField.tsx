import { GitBranch } from "lucide-react";

import {
  forgeTeamRepoKey,
  type ForgeTeamRepo,
} from "@/api/forgeConnections";
import type { ExternalLink } from "@/api/native";
import { Select } from "@/components/ui/Select";

import { Field } from "./Field";

interface RepositoryFieldProps {
  repos: ForgeTeamRepo[];
  /** forgeTeamRepoKey of the picked repo, or "" for "No repository". */
  value: string;
  onChange: (key: string) => void;
  /** When set, the card is already forge-linked (number/url) and can't be
   *  re-linked; renders a read-only "synced from forge" note instead. */
  synced?: ExternalLink | null;
  /** When the currently-linked repo isn't in the connected list (repo was
   *  disconnected since sync), still surface it as a synthetic option so
   *  the operator doesn't accidentally wipe the link. */
  legacyLinkedLabel?: string | null;
}

// RepositoryField renders the IssueModal's repo-first scoping picker: a
// Select of connected repos + "No repository", or a read-only note when the
// card is already synced from its forge (number/url present).
export function RepositoryField({
  repos,
  value,
  onChange,
  synced,
  legacyLinkedLabel,
}: RepositoryFieldProps) {
  if (synced) {
    return (
      <Field label="Repository">
        <div className="flex items-center gap-2 text-xs text-fg-muted rounded-md border border-border-default bg-surface-1 px-2.5 py-2">
          <GitBranch className="h-3.5 w-3.5 shrink-0 opacity-70" aria-hidden="true" />
          <span className="text-fg-default font-medium">{synced.repo}</span>
          {synced.number > 0 && (
            <span className="font-mono opacity-70">#{synced.number}</span>
          )}
          <span className="ml-auto text-fg-subtle italic">
            synced from {synced.provider}
          </span>
        </div>
      </Field>
    );
  }
  return (
    <Field
      label="Repository"
      help="Scope this card to a connected forge repository (the board's repo filter uses this)."
    >
      <Select
        value={value}
        onChange={(e) => onChange(e.target.value)}
        size="md"
        aria-label="Repository"
      >
        <option value="">No repository</option>
        {repos.map((r) => (
          <option key={forgeTeamRepoKey(r)} value={forgeTeamRepoKey(r)}>
            {r.repo_full_name}
          </option>
        ))}
        {/* Preserve a link to a repo the operator has since disconnected —
            avoids silently losing the linkage on save. */}
        {legacyLinkedLabel && !repos.some((r) => forgeTeamRepoKey(r) === value) && (
          <option value={value}>{legacyLinkedLabel} (disconnected)</option>
        )}
      </Select>
    </Field>
  );
}
