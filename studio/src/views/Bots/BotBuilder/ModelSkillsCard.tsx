// Extracted from BotBuilder/index.tsx to keep that file focused.
// The compact model & skills group — backend/model pickers fed by the
// live backend-detection report, plus the attachable-skill chips from
// the local skill library.

import { useEffect, useMemo } from "react";
import { Cross1Icon } from "@radix-ui/react-icons";
import { useQuery } from "@tanstack/react-query";

import { FeatureUnavailableError } from "@/api/client";
import { listLocalSkills, type LibrarySkill } from "@/api/skills";
import { Card, FieldLabel, Input, Select, Spinner } from "@/components/ui";
import { errorMessage } from "@/lib/errorHints";
import { useBackendDetectStore } from "@/store/backendDetect";

import { ModelCapsCaption } from "@/components/ModelCapsCaption";
import { type BuilderDraft, type PatchDraft } from "./model";
import SectionTitle from "./SectionTitle";

export default function ModelSkillsCard({
  draft,
  patch,
}: {
  draft: BuilderDraft;
  patch: PatchDraft;
}) {
  const report = useBackendDetectStore((s) => s.report);
  const detectLoading = useBackendDetectStore((s) => s.loading);
  const refreshDetect = useBackendDetectStore((s) => s.refresh);
  useEffect(() => {
    if (!report && !detectLoading) void refreshDetect();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // A failed load renders as an empty catalog + a note; the library being
  // absent on this server (FeatureUnavailableError) is expected, not an error.
  const skillsQuery = useQuery<LibrarySkill[]>({
    queryKey: ["local-skills"],
    queryFn: () => listLocalSkills(),
  });
  const skillCatalog = skillsQuery.data ?? (skillsQuery.error ? [] : null);
  const skillsNote = skillsQuery.error
    ? skillsQuery.error instanceof FeatureUnavailableError
      ? "The skills library isn't available on this server — skills can't be browsed here."
      : `Couldn't load the skill library: ${errorMessage(skillsQuery.error)}`
    : null;

  const toggleSkill = (name: string) =>
    patch({
      skills: draft.skills.includes(name)
        ? draft.skills.filter((s) => s !== name)
        : [...draft.skills, name],
    });

  const modelSuggestions = useMemo(() => {
    const set = new Set<string>();
    for (const p of report?.providers ?? []) {
      if (p.available && p.suggested_model) set.add(p.suggested_model);
    }
    return [...set].sort();
  }, [report]);
  const suggestionsId = "bot-builder-model-suggestions";

  // Skills referenced by the draft (e.g. from a template) that the
  // catalog doesn't know — keep them visible + removable.
  const knownNames = new Set((skillCatalog ?? []).map((s) => s.name));
  const orphanSkills = draft.skills.filter((s) => !knownNames.has(s));

  return (
    <Card>
      <SectionTitle>Model &amp; skills</SectionTitle>

      <div className="mt-2 grid gap-3 sm:grid-cols-2">
        <div>
          <FieldLabel htmlFor="bot-backend">Backend</FieldLabel>
          <Select
            id="bot-backend"
            value={draft.backend}
            onChange={(e) => patch({ backend: e.currentTarget.value })}
          >
            <option value="">
              Auto (detected{report?.resolved_default ? ` — ${report.resolved_default}` : ""})
            </option>
            {(report?.backends ?? []).map((b) => (
              <option key={b.name} value={b.name} disabled={!b.available}>
                {b.name}
                {b.available ? "" : " — no credential"}
              </option>
            ))}
          </Select>
        </div>
        <div>
          <FieldLabel htmlFor="bot-model">Model</FieldLabel>
          <Input
            id="bot-model"
            type="text"
            list={suggestionsId}
            value={draft.model}
            onChange={(e) => patch({ model: e.target.value })}
            placeholder="Auto (detected)"
            className="font-mono"
          />
          <datalist id={suggestionsId}>
            {modelSuggestions.map((m) => (
              <option key={m} value={m} />
            ))}
          </datalist>
          <ModelCapsCaption override={draft.model} />
        </div>
      </div>
      <p className="mt-1.5 text-caption text-fg-subtle">
        Leave both empty for auto-detection from the host&apos;s credentials — the recommended
        default.
      </p>

      <div className="mt-3">
        <FieldLabel>Skills</FieldLabel>
        {skillsNote && (
          <p className="mb-1.5 text-caption text-warning" role="note">
            {skillsNote}
          </p>
        )}
        {skillCatalog === null && !skillsNote ? (
          <div className="flex items-center gap-2 py-1 text-xs text-fg-muted">
            <Spinner size="sm" /> Loading skills…
          </div>
        ) : (
          <>
            {(skillCatalog ?? []).length === 0 && !skillsNote && (
              <p className="text-caption text-fg-subtle">
                No skills in the library yet — add some under the Skills view and they become
                attachable here.
              </p>
            )}
            <div className="flex flex-wrap gap-1.5">
              {(skillCatalog ?? []).map((s) => {
                const selected = draft.skills.includes(s.name);
                return (
                  <button
                    key={`${s.scope}:${s.name}`}
                    type="button"
                    onClick={() => toggleSkill(s.name)}
                    aria-pressed={selected}
                    title={s.description || s.name}
                    className={`rounded-full border px-2 py-0.5 font-mono text-caption transition-colors ${
                      selected
                        ? "border-accent bg-accent-soft text-fg-default"
                        : "border-border-default bg-surface-2 text-fg-muted hover:border-border-strong hover:text-fg-default"
                    }`}
                  >
                    {selected ? "☑ " : ""}
                    {s.name}
                  </button>
                );
              })}
              {orphanSkills.map((name) => (
                <span
                  key={name}
                  className="inline-flex items-center gap-1 rounded-full border border-border-default bg-surface-2 px-2 py-0.5 font-mono text-caption text-fg-muted"
                >
                  {name}
                  <button
                    type="button"
                    onClick={() => toggleSkill(name)}
                    aria-label={`Remove skill ${name}`}
                    className="text-fg-subtle hover:text-fg-default"
                  >
                    <Cross1Icon className="h-2.5 w-2.5" />
                  </button>
                </span>
              ))}
            </div>
          </>
        )}
      </div>
    </Card>
  );
}
