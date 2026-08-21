import { useCallback } from "react";
import { Cross2Icon, PlusIcon } from "@radix-ui/react-icons";

import type { FallbackDecl } from "@/api/types";
import { Button, IconButton } from "@/components/ui";
import {
  BACKEND_OPTIONS,
  FALLBACKS_HELP,
  FALLBACK_ON_OPTIONS,
} from "@/lib/dslOptions";
import { displayModel } from "@/lib/modelLabel";
import { useResolvedModels } from "@/hooks/useResolvedModel";
import { CheckboxField, SelectField, TagListField, TextField } from "./FormField";

interface Props {
  value: FallbackDecl[] | undefined;
  onChange: (next: FallbackDecl[] | undefined) => void;
}

function uniqueRouteName(existing: string[]): string {
  const used = new Set(existing);
  if (!used.has("api")) return "api";
  let i = 2;
  while (used.has(`route_${i}`)) i += 1;
  return `route_${i}`;
}

/** Per-node `fallbacks:` editor (ADR-087). Hidden until the node has
 *  a route or the operator opens the section, so workflows that don't
 *  use the feature stay uncluttered. */
export default function FallbacksFields({ value, onChange }: Props) {
  const fallbacks = value ?? [];
  const resolved = useResolvedModels(fallbacks.map((fb) => fb.model));

  const commit = useCallback(
    (next: FallbackDecl[]) => {
      onChange(next.length > 0 ? next : undefined);
    },
    [onChange],
  );

  const patch = useCallback(
    (index: number, updates: Partial<FallbackDecl>) => {
      commit(fallbacks.map((fb, i) => (i === index ? { ...fb, ...updates } : fb)));
    },
    [commit, fallbacks],
  );

  const remove = useCallback(
    (index: number) => {
      commit(fallbacks.filter((_, i) => i !== index));
    },
    [commit, fallbacks],
  );

  const add = useCallback(() => {
    commit([
      ...fallbacks,
      { name: uniqueRouteName(fallbacks.map((fb) => fb.name)) },
    ]);
  }, [commit, fallbacks]);

  return (
    <details
      className="border-t border-border-default pt-2 mt-2"
      {...(fallbacks.length > 0 ? { open: true } : {})}
    >
      <summary className="cursor-pointer text-xs text-fg-subtle font-semibold mb-1">
        Fallbacks
        {fallbacks.length > 0 ? (
          <span className="text-fg-subtle font-normal"> ({fallbacks.length})</span>
        ) : null}
      </summary>
      <p className="text-caption text-fg-muted mb-2">{FALLBACKS_HELP}</p>
      <div className="pl-2 space-y-2">
        {fallbacks.map((fb, i) => {
          const live = resolved[i];
          const shown = displayModel(fb.model, live);
          return (
            <div
              key={`${fb.name}-${i}`}
              className="border border-border-default rounded p-2 space-y-1"
            >
              <div className="flex items-center justify-between gap-2 mb-1">
                <span className="text-caption text-fg-muted">
                  {i + 1}. {fb.name || "(unnamed)"}
                  {shown ? ` → ${shown}` : ""}
                </span>
                <IconButton
                  variant="ghost"
                  size="sm"
                  label={`Remove fallback ${fb.name || i + 1}`}
                  tooltip="Remove route"
                  onClick={() => remove(i)}
                >
                  <Cross2Icon />
                </IconButton>
              </div>
              <TextField
                label="Name"
                value={fb.name}
                onChange={(v) => patch(i, { name: v.trim() })}
                placeholder="api"
                help="Stable id cited by the fall-through event. Required."
              />
              <SelectField
                label="Backend"
                value={fb.backend ?? ""}
                onChange={(v) => patch(i, { backend: v || undefined })}
                options={BACKEND_OPTIONS}
                help="Empty inherits this node's backend."
              />
              <TextField
                label="Model"
                value={fb.model ?? ""}
                onChange={(v) => patch(i, { model: v || undefined })}
                placeholder="e.g. openai/gpt-5.5"
                help="Required when backend changes — model specs are not portable across backends."
              />
              <TextField
                label="Provider"
                value={fb.provider ?? ""}
                onChange={(v) => patch(i, { provider: v || undefined })}
                placeholder="(auto)"
                help="Credential-routing hint. Empty = process-env precedence."
              />
              <TagListField
                label="On (triggers)"
                values={fb.on ?? []}
                onChange={(v) => patch(i, { on: v.length > 0 ? v : undefined })}
                placeholder={FALLBACK_ON_OPTIONS.map((o) => o.value).join(", ")}
              />
              <CheckboxField
                label="Metered"
                checked={!!fb.metered}
                onChange={(v) => patch(i, { metered: v || undefined })}
                help="Author's acknowledgement that this route spends a metered credential."
              />
            </div>
          );
        })}
        <Button
          type="button"
          variant="ghost"
          size="sm"
          leadingIcon={<PlusIcon />}
          onClick={add}
        >
          Add route
        </Button>
      </div>
    </details>
  );
}
