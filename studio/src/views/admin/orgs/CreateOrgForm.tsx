// "Create an organization" section of the admin console. Owns its two
// input drafts; submission rides the page's shared run() busy/error slot.

import { useState } from "react";

import { createOrg } from "@/api/orgs";
import { Button } from "@/components/ui/Button";
import { Input } from "@/components/ui/Input";

export function CreateOrgForm({
  busy,
  run,
  reloadIdentity,
}: {
  busy: boolean;
  run: (fn: () => Promise<unknown>) => Promise<void>;
  reloadIdentity: () => Promise<void>;
}) {
  const [name, setName] = useState("");
  const [ownerEmail, setOwnerEmail] = useState("");

  const create = (e: React.FormEvent) => {
    e.preventDefault();
    if (!name.trim()) return;
    void run(async () => {
      await createOrg({ name: name.trim(), owner_email: ownerEmail.trim() || undefined });
      setName("");
      setOwnerEmail("");
      // Refresh the AuthContext org tree (it feeds the org switcher in
      // UserTeamChip), not just this page's local table — otherwise the new
      // org only appears after a full page reload.
      await reloadIdentity();
    });
  };

  return (
    <section className="bg-surface-1 border border-border-subtle rounded-[var(--radius-lg)] shadow-[var(--shadow-sm)] p-4 space-y-3">
      <h3 className="font-medium">Create an organization</h3>
      <form onSubmit={create} className="flex flex-wrap gap-2 items-start">
        <div className="flex-1 min-w-[160px]">
          <label htmlFor="create-org-name" className="sr-only">
            Org name
          </label>
          <Input
            size="md"
            id="create-org-name"
            placeholder="Org name"
            value={name}
            onChange={(e) => setName(e.target.value)}
            required
          />
        </div>
        <div className="flex-1 min-w-[200px]">
          <label htmlFor="create-org-owner" className="sr-only">
            Owner email
          </label>
          <Input
            size="md"
            type="email"
            id="create-org-owner"
            placeholder="owner email (optional)"
            value={ownerEmail}
            onChange={(e) => setOwnerEmail(e.target.value)}
          />
        </div>
        <Button variant="primary" type="submit" loading={busy}>
          Create
        </Button>
      </form>
    </section>
  );
}
