import { useState } from "react";

import { Checkbox } from "@/components/ui/Checkbox";
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

      <p className="text-caption text-fg-subtle">
        Both are remembered in this browser, and only offered for assistants
        whose bundle declares that it supports them.
      </p>
    </div>
  );
}
