import { useState } from "react";

import { Checkbox } from "@/components/ui/Checkbox";
import { Select } from "@/components/ui/Select";
import {
  ASSISTANT_ACTIONS,
  ASSISTANT_ACTION_POLICY_OPTIONS,
  type AssistantActionDefinition,
  type AssistantActionPolicy,
  useAssistantActionPolicy,
  writeAssistantActionPolicy,
} from "@/lib/chatDock/assistantActions";
import {
  readAskBeforeStart,
  readReviewer,
  writeAskBeforeStart,
  writeReviewer,
} from "@/lib/chatDock/assistantPrefs";

// The home for choices the dock offers before a conversation starts.
//
// It exists because of the "don't ask again" checkbox: a setting you can only
// reach in a prompt you dismissed is a setting you cannot change. Dismissing
// the prompt must mean "stop asking", never "you are stuck with this".
export default function AssistantTab() {
  const [reviewer, setReviewer] = useState(readReviewer);
  const [ask, setAsk] = useState(readAskBeforeStart);

  return (
    <div className="space-y-5 text-label text-fg-default">
      <section className="space-y-2">
        <h3 className="text-label font-medium">Cross-review</h3>
        <Checkbox
          checked={reviewer}
          onChange={(e) => {
            setReviewer(e.target.checked);
            writeReviewer(e.target.checked);
          }}
          label="Cross-review each answer by default"
        />
        <p className="text-caption text-fg-subtle">
          A second model, from another family, criticises each answer before you
          read it. It catches real mistakes — and costs a full extra model call
          per turn, which is why it is off by default. Applies to new
          conversations; one already running keeps the choice it started with.
        </p>
      </section>

      <section className="space-y-2">
        <h3 className="text-label font-medium">Starting a conversation</h3>
        <Checkbox
          checked={ask}
          onChange={(e) => {
            setAsk(e.target.checked);
            writeAskBeforeStart(e.target.checked);
          }}
          label="Ask before starting a new conversation"
        />
        <p className="text-caption text-fg-subtle">
          Shows the options above in the assistant before the first message.
          Turn it off to start straight away with the setting saved here.
        </p>
      </section>

      <section className="space-y-3">
        <div className="space-y-1">
          <h3 className="text-label font-medium">Actions</h3>
          <p className="text-caption text-fg-subtle">
            Choose how much autonomy the assistant has for each host-controlled
            action. These settings never bypass your permissions, validation,
            revision checks, or read-only files.
          </p>
        </div>
        <div className="divide-y divide-border-subtle rounded-md border border-border-subtle">
          {ASSISTANT_ACTIONS.map((action) => (
            <ActionPolicyRow key={action.id} action={action} />
          ))}
        </div>
        <p className="text-caption text-fg-subtle">
          “Explicitly requested” means the action may run automatically only
          when your current request asks for it. “Always allow” lets the
          assistant include it as part of a task. Newly added actions always
          start at “Always ask”.
        </p>
      </section>

      <p className="text-caption text-fg-subtle">
        These choices are remembered in this browser. Cross-review options are
        only offered for assistants whose bundle declares that it supports
        them; action policies are enforced by the Studio for every assistant.
      </p>
    </div>
  );
}

function ActionPolicyRow({ action }: { action: AssistantActionDefinition }) {
  const policy = useAssistantActionPolicy(action.id);
  return (
    <div className="grid gap-2 p-3 sm:grid-cols-[minmax(0,1fr)_14rem] sm:items-center">
      <div className="min-w-0">
        <div className="flex items-center gap-2">
          <p className="text-label font-medium">{action.label}</p>
          <span className="rounded-full border border-border-subtle px-1.5 py-0.5 text-micro uppercase tracking-wide text-fg-subtle">
            {action.risk}
          </span>
        </div>
        <p className="mt-0.5 text-caption text-fg-subtle">
          {action.description}
        </p>
      </div>
      <Select
        aria-label={`${action.label} policy`}
        value={policy}
        onChange={(event) =>
          writeAssistantActionPolicy(
            action.id,
            event.target.value as AssistantActionPolicy,
          )
        }
      >
        {ASSISTANT_ACTION_POLICY_OPTIONS.map((option) => (
          <option key={option.value} value={option.value}>
            {option.label}
          </option>
        ))}
      </Select>
    </div>
  );
}
