// Extracted from RunHeader.tsx to keep that file focused.
// Row: operator-assigned filter/group tags ("release", "flaky",
// "customer-x") as editable chips. Persisted per-run via
// GET/PUT /api/runs/:id/tags — a whole-list overwrite on every edit.

import { useEffect, useState } from "react";

import { getRunTags, setRunTags } from "@/api/runs";
import { TagInput } from "@/components/ui";
import { errorMessage } from "@/lib/errorHints";

// Mirror the server-side caps (store.MaxTagLen / store.MaxTagsPerRun) so the
// UI never sends a list the PUT would 400 on.
const MAX_TAG_LENGTH = 32;
const MAX_TAGS = 20;

// RunTagsRow loads the run's tags on mount and persists the full set on
// each add/remove. Edits are optimistic: the chips update immediately and
// revert with an error line if the PUT fails. A run with no tags still
// renders the (empty) input so the operator can add the first tag.
export default function RunTagsRow({ runId }: { runId: string }) {
  const [tags, setTags] = useState<string[]>([]);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    getRunTags(runId)
      .then((t) => {
        if (!cancelled) setTags(t);
      })
      .catch(() => {
        // A load failure leaves the row empty; the operator can still add
        // tags (the PUT surfaces its own error). Don't spam the header.
      });
    return () => {
      cancelled = true;
    };
  }, [runId]);

  const onChange = (next: string[]) => {
    if (next.length > MAX_TAGS) {
      setError(`At most ${MAX_TAGS} tags per run.`);
      return;
    }
    const prev = tags;
    setTags(next); // optimistic
    setError(null);
    setRunTags(runId, next)
      .then((persisted) => setTags(persisted))
      .catch((e) => {
        setTags(prev); // revert
        setError(errorMessage(e));
      });
  };

  return (
    <div className="flex items-start gap-2 text-micro text-fg-subtle">
      <span className="mt-1.5 shrink-0 uppercase tracking-wide text-[0.65rem]">
        Tags
      </span>
      <div className="min-w-0 flex-1">
        <TagInput
          value={tags}
          onChange={onChange}
          placeholder="Add tag (release, flaky, customer-x…)"
          maxTagLength={MAX_TAG_LENGTH}
        />
        {error && <p className="mt-1 text-danger-fg">{error}</p>}
      </div>
    </div>
  );
}
