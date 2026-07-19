import { useCallback, useEffect, useState } from "react";
import { MixIcon } from "@radix-ui/react-icons";
import { useQuery, useQueryClient } from "@tanstack/react-query";

import {
  type LibrarySkill,
  createLocalSkill,
  deleteLocalSkill,
  getLocalSkill,
  isValidSkillName,
  listLocalSkills,
  updateLocalSkill,
} from "@/api/skills";
import { errorMessage } from "@/lib/errorHints";
import { useAsyncAction } from "@/hooks/useAsyncAction";
import { useConfirm } from "@/hooks/useConfirm";
import { Badge } from "@/components/ui/Badge";
import { Button } from "@/components/ui/Button";
import { Dialog } from "@/components/ui/Dialog";
import { EmptyState } from "@/components/ui/EmptyState";
import { InlineBanner } from "@/components/ui/InlineBanner";
import { Input } from "@/components/ui/Input";
import { Select } from "@/components/ui/Select";
import { Table, THead, Th, TBody, Tr, Td, TableSkeleton } from "@/components/ui/Table";
import { Textarea } from "@/components/ui/Textarea";
import { PageHeader } from "@/components/ui/PageHeader";

const NEW_SKILL_TEMPLATE = `---
name: my-skill
description: One line summarising when to use this skill.
---

# My skill

Imperative guidance the agent follows when this skill is loaded.
`;

// Skills manages the local (non-cloud) skill library: machine-global
// ~/.iterion/skills plus an optional per-project override. Skills are plain
// markdown (SKILL.md), referenced from a workflow's `skills:` field and
// mirrored into a run's .claude/skills/ at launch. Backed by /api/local/skills
// (server_info.skills_enabled).
// Stable empty fallback for the errored state, so renders don't hand the
// table a fresh [] reference each time.
const EMPTY_SKILLS: LibrarySkill[] = [];

export default function Skills() {
  const queryClient = useQueryClient();
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<LibrarySkill | null>(null);
  const { confirm, dialog } = useConfirm();

  const skillsQuery = useQuery<LibrarySkill[]>({
    queryKey: ["local-skills"],
    queryFn: listLocalSkills,
  });
  // On a fetch error the list reads as empty (banner over the empty
  // state, not a stale table or an endless skeleton).
  const skills = skillsQuery.isError ? EMPTY_SKILLS : (skillsQuery.data ?? null);
  // Delete failures share the fetch error's banner; any reload clears
  // them (the fetch side clears itself on refetch).
  const [actionError, setActionError] = useState<string | null>(null);
  const error =
    actionError ??
    (skillsQuery.error && !skillsQuery.isFetching
      ? errorMessage(skillsQuery.error)
      : null);

  // Post-mutation reload (delete / create / edit): invalidate so the
  // list refetches.
  const reload = useCallback(() => {
    setActionError(null);
    void queryClient.invalidateQueries({ queryKey: ["local-skills"] });
  }, [queryClient]);

  const doDelete = async (rec: LibrarySkill) => {
    const ok = await confirm({
      title: `Delete ${rec.name}?`,
      message:
        "Bots that reference this skill by name will no longer have it mirrored into their runs.",
      confirmLabel: "Delete",
      confirmVariant: "danger",
    });
    if (!ok) return;
    try {
      await deleteLocalSkill(rec.name, rec.scope);
      reload();
    } catch (e) {
      setActionError(errorMessage(e));
    }
  };

  return (
    <div className="flex h-full min-h-0 flex-col bg-surface-0 text-fg-default">
      <PageHeader
        icon={<MixIcon className="h-5 w-5" />}
        title="Skills"
        description={
          <>
            A curated library of reusable skills. A bot references one by name in
            its <code>skills:</code> field; at launch iterion mirrors it into the
            run's <code>.claude/skills/</code> and lists it under a{" "}
            <code>## Skills</code> prompt section. Project skills override global
            ones by name.
          </>
        }
        actions={
          <div className="flex items-center gap-2">
            <Button
              variant="secondary"
              size="sm"
              onClick={() => {
                setActionError(null);
                void skillsQuery.refetch();
              }}
            >
              Refresh
            </Button>
            <Button variant="primary" size="sm" onClick={() => setCreating(true)}>
              Add skill
            </Button>
          </div>
        }
      />

      <div className="mx-auto flex w-full max-w-3xl flex-1 flex-col gap-3 overflow-y-auto p-6">
        {error && (
          <InlineBanner tone="danger" title="Skills error" layout="inline">
            {error}
          </InlineBanner>
        )}

        {skills === null ? (
          <TableSkeleton rows={4} cols={4} />
        ) : skills.length === 0 ? (
          <EmptyState
            title="No skills yet"
            message="Add a skill, then reference it from a bot's skills: field by name."
          />
        ) : (
          <SkillsTable
            skills={skills}
            onEdit={setEditing}
            onDelete={(rec) => void doDelete(rec)}
          />
        )}
      </div>

      {creating && (
        <UpsertSkillDialog
          onClose={() => setCreating(false)}
          onDone={() => {
            setCreating(false);
            reload();
          }}
        />
      )}

      {editing && (
        <UpsertSkillDialog
          rec={editing}
          onClose={() => setEditing(null)}
          onDone={() => {
            setEditing(null);
            reload();
          }}
        />
      )}

      {dialog}
    </div>
  );
}

function SkillsTable({
  skills,
  onEdit,
  onDelete,
}: {
  skills: LibrarySkill[];
  onEdit: (rec: LibrarySkill) => void;
  onDelete: (rec: LibrarySkill) => void;
}) {
  return (
    <Table caption="Skill library">
      <THead>
        <Th>Name</Th>
        <Th>Scope</Th>
        <Th>Description</Th>
        <Th align="right">Actions</Th>
      </THead>
      <TBody>
        {skills.map((s) => (
          <Tr key={`${s.scope}:${s.name}`}>
            <Td className="font-mono">{s.name}</Td>
            <Td>
              <Badge variant={s.scope === "project" ? "info" : "neutral"}>{s.scope}</Badge>
            </Td>
            <Td className="text-fg-muted">{s.description || "—"}</Td>
            <Td align="right" className="space-x-1 whitespace-nowrap">
              <Button size="sm" variant="ghost" onClick={() => onEdit(s)}>
                Edit
              </Button>
              <Button size="sm" variant="ghost" className="text-danger" onClick={() => onDelete(s)}>
                Delete
              </Button>
            </Td>
          </Tr>
        ))}
      </TBody>
    </Table>
  );
}

// UpsertSkillDialog creates a new skill or edits an existing one (when rec is
// set). On edit, name + scope are fixed; only the body changes. On edit the
// body is fetched lazily (the list omits it).
function UpsertSkillDialog({
  rec,
  onClose,
  onDone,
}: {
  rec?: LibrarySkill;
  onClose: () => void;
  onDone: () => void;
}) {
  const edit = rec !== undefined;
  const [name, setName] = useState(rec?.name ?? "");
  const [scope, setScope] = useState<"global" | "project">(rec?.scope ?? "global");
  const [body, setBody] = useState(edit ? "" : NEW_SKILL_TEMPLATE);
  const [loadingBody, setLoadingBody] = useState(edit);
  const { busy, error: err, run } = useAsyncAction();

  useEffect(() => {
    if (!edit || !rec) return;
    let cancelled = false;
    void getLocalSkill(rec.name)
      .then((full) => {
        if (!cancelled) setBody(full.body ?? "");
      })
      .finally(() => {
        if (!cancelled) setLoadingBody(false);
      });
    return () => {
      cancelled = true;
    };
  }, [edit, rec]);

  const nameOk = edit || isValidSkillName(name);
  const canSubmit = nameOk && body.trim() !== "" && !loadingBody;

  const submit = () => {
    if (!canSubmit) return;
    return run(async () => {
      if (edit && rec) {
        await updateLocalSkill(rec.name, { body, scope: rec.scope });
      } else {
        await createLocalSkill({ name, body, scope });
      }
      onDone();
    });
  };

  return (
    <Dialog
      open
      onOpenChange={(o) => {
        if (!o) onClose();
      }}
      title={edit ? `Edit ${rec.name}` : "Add skill"}
      description="Skills are plain markdown with optional YAML frontmatter (name, description)."
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" disabled={!canSubmit || busy} onClick={() => void submit()}>
            {edit ? "Save" : "Create"}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        {err && (
          <InlineBanner tone="danger" title="Save failed" layout="inline">
            {err}
          </InlineBanner>
        )}
        {!edit && (
          <div className="flex gap-3">
            <div className="flex-1">
              <label className="mb-1 block text-xs text-fg-muted">Name</label>
              <Input
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="changelog-writer"
                error={name !== "" && !nameOk}
              />
              {name !== "" && !nameOk && (
                <p className="mt-1 text-xs text-danger">
                  A single segment: letters, digits, . - _ — not starting with a dot.
                </p>
              )}
            </div>
            <div className="w-32">
              <label className="mb-1 block text-xs text-fg-muted">Scope</label>
              <Select
                size="md"
                value={scope}
                onChange={(e) => setScope(e.target.value as "global" | "project")}
              >
                <option value="global">global</option>
                <option value="project">project</option>
              </Select>
            </div>
          </div>
        )}
        <div>
          <label className="mb-1 block text-xs text-fg-muted">SKILL.md</label>
          <Textarea
            value={body}
            onChange={(e) => setBody(e.target.value)}
            rows={16}
            className="font-mono text-xs"
            placeholder={loadingBody ? "Loading…" : NEW_SKILL_TEMPLATE}
            disabled={loadingBody}
          />
        </div>
      </div>
    </Dialog>
  );
}
