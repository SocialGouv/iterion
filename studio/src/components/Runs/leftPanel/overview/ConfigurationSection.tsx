import { type ReactNode } from "react";

import { ProviderIcon } from "@/components/icons/ProviderIcon";
import type { RunHeader, RunModelOverride } from "@/api/runs";

import { Row, Section } from "../InfoPrimitives";

interface ConfigurationSectionProps {
  run: RunHeader;
}

// ConfigurationSection is the compact "how was this run configured?"
// summary — bot, permission gate, model pins, worktree/merge strategy,
// source ticket. Collapsed by default: it's orientation info, read once,
// not a live vital. Only fields ACTUALLY carried on the RunHeader wire
// are rendered (model_overrides is persisted; compress/other options ride
// the launch request, not the header, so we don't fabricate rows).
export function ConfigurationSection({ run }: ConfigurationSectionProps) {
  const fields = collectLaunchedWith(run);
  if (fields.length === 0) return null;
  return (
    <Section title="Launched with" collapsible defaultOpen={false}>
      {fields.map((f) => (
        <Row key={f.label} label={f.label}>
          {f.render}
        </Row>
      ))}
    </Section>
  );
}

interface LaunchedWithField {
  label: string;
  render: ReactNode;
}

function collectLaunchedWith(run: RunHeader): LaunchedWithField[] {
  const fields: LaunchedWithField[] = [];

  if (run.bundle_display_name || run.bundle_name) {
    fields.push({
      label: "Bot",
      render: (
        <span className="truncate">
          {run.bundle_display_name || run.bundle_name}
        </span>
      ),
    });
  }

  if (run.permission_mode && run.permission_mode !== "off") {
    fields.push({
      label: "Permission",
      render: <span>{run.permission_mode}</span>,
    });
  }

  if (run.model_overrides && run.model_overrides.length > 0) {
    fields.push({
      label: run.model_overrides.length > 1 ? "Models" : "Model",
      render: (
        <span className="flex flex-col gap-0.5">
          {run.model_overrides.map((o, i) => (
            <span
              key={i}
              className="inline-flex items-center gap-1 font-mono text-caption break-words"
            >
              <ProviderIcon
                model={o.model}
                delegate={o.backend}
                size={12}
                className="shrink-0"
              />
              {formatModelOverride(o)}
            </span>
          ))}
        </span>
      ),
    });
  }

  fields.push({
    label: "Worktree",
    render: (
      <span>
        {run.worktree
          ? "auto (fresh git worktree)"
          : "off (project working dir)"}
      </span>
    ),
  });

  if (run.worktree) {
    fields.push({
      label: "Merge",
      render: (
        <span>
          {run.merge_strategy || "squash"}
          {" · "}
          {run.auto_merge ? "auto-merge on" : "auto-merge off"}
        </span>
      ),
    });
  }

  if (run.source?.kind) {
    fields.push({
      label: "Source",
      render: <span>{formatSource(run.source)}</span>,
    });
  }

  return fields;
}

// formatModelOverride renders one override rule as "<selector> → <pins>",
// pins being the non-empty subset of model / backend / provider.
function formatModelOverride(o: RunModelOverride): string {
  const pins = [o.model, o.backend, o.provider].filter(Boolean).join(" · ");
  return pins ? `${o.selector} → ${pins}` : o.selector;
}

function formatSource(source: NonNullable<RunHeader["source"]>): string {
  const kind = source.kind ?? "manual";
  if (source.issue_identifier) return `${kind} · ${source.issue_identifier}`;
  if (source.issue_title) return `${kind} · ${source.issue_title}`;
  const schedule = source.schedule_name || source.schedule_id;
  if (schedule) return `${kind} · ${schedule}`;
  return kind;
}
