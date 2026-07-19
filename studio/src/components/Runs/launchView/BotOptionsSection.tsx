// "Bot options" — the bot's own tunable inputs below the primary form:
// optional vars with defaults plus the auto-managed (runner-resolved)
// rows. Sibling of EngineOptionsSection, which holds the iterion engine
// knobs; the split keeps a bot's domain inputs from drowning in
// backend/budget/worktree tuning. Omitted entirely when the bot declares
// no such vars.

import type { VarField } from "@/api/types";

import AutoManagedVarsSection from "./AutoManagedVarsSection";
import OptionsDisclosure from "./OptionsDisclosure";
import VarFieldsSection from "./VarFieldsSection";

export interface BotOptionsSectionProps {
  advancedVarFields: VarField[];
  autoManagedFields: VarField[];
  values: Record<string, string>;
  submitting: boolean;
  onValueChange: (name: string, value: string) => void;
  onSubmit: () => void;
  open: boolean;
  onToggle: () => void;
}

export default function BotOptionsSection({
  advancedVarFields,
  autoManagedFields,
  values,
  submitting,
  onValueChange,
  onSubmit,
  open,
  onToggle,
}: BotOptionsSectionProps) {
  const count = advancedVarFields.length + autoManagedFields.length;
  if (count === 0) return null;
  return (
    <OptionsDisclosure
      label="Bot options"
      count={count}
      hint="optional inputs"
      open={open}
      onToggle={onToggle}
    >
      <div className="mt-3">
        <VarFieldsSection
          fields={advancedVarFields}
          title="Optional inputs"
          values={values}
          submitting={submitting}
          onValueChange={onValueChange}
          onSubmit={onSubmit}
        />
        <AutoManagedVarsSection
          fields={autoManagedFields}
          values={values}
          submitting={submitting}
          onValueChange={onValueChange}
        />
      </div>
    </OptionsDisclosure>
  );
}
