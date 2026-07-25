// Extracted from BotBuilder/index.tsx to keep that file focused.
// Phase 1 of the guided builder — the template gallery
// (GET /api/v1/bots/templates) with the resume-draft banner and the
// "start blank" escape hatch.

import { useQuery } from "@tanstack/react-query";

import { listBotTemplates, type BotTemplate } from "@/api/bots";
import { Button, InlineBanner, Spinner } from "@/components/ui";
import { errorMessage } from "@/lib/errorHints";

export default function TemplateGallery({
  onPick,
  onSkip,
  hasDraft,
  onResumeDraft,
}: {
  onPick: (t: BotTemplate) => void;
  onSkip: () => void;
  hasDraft: boolean;
  onResumeDraft: () => void;
}) {
  const templatesQuery = useQuery<BotTemplate[]>({
    queryKey: ["bot-templates"],
    queryFn: () => listBotTemplates(),
  });
  const templates = templatesQuery.data ?? null;
  // Hidden while a retry is in flight so the loading state shows instead.
  const error =
    templatesQuery.error && !templatesQuery.isFetching
      ? errorMessage(templatesQuery.error)
      : null;

  return (
    <div className="mx-auto w-full max-w-4xl p-4">
      <h1 className="text-base font-semibold text-fg-default">Create a bot</h1>
      <p className="mt-1 text-xs text-fg-muted">
        Pick a starting point — every template pre-fills the form and stays fully editable.
      </p>

      {hasDraft && (
        <div className="mt-3">
          <InlineBanner tone="info" title="You have an unfinished draft">
            <Button variant="secondary" size="sm" onClick={onResumeDraft}>
              Resume draft
            </Button>
          </InlineBanner>
        </div>
      )}

      {error && (
        <div className="mt-3">
          <InlineBanner tone="danger" title="Couldn't load templates">
            {error}
            <div className="mt-2">
              <Button
                variant="secondary"
                size="sm"
                onClick={() => void templatesQuery.refetch()}
              >
                Retry
              </Button>
            </div>
          </InlineBanner>
        </div>
      )}

      {templates === null && !error ? (
        <div className="mt-4 flex items-center gap-2 text-sm text-fg-muted">
          <Spinner /> Loading templates…
        </div>
      ) : (
        <div className="mt-4 grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {(templates ?? []).map((t) => (
            <button
              key={t.id}
              type="button"
              onClick={() => onPick(t)}
              className="flex flex-col items-start gap-1.5 rounded-md border border-border-default bg-surface-1 p-3 text-left transition-colors hover:border-accent hover:bg-surface-2 focus-visible:border-accent"
            >
              <span className="text-2xl leading-none" aria-hidden="true">
                {t.icon || "🤖"}
              </span>
              <span className="text-xs font-semibold text-fg-default">{t.name}</span>
              <span className="text-caption text-fg-muted">{t.description}</span>
            </button>
          ))}
        </div>
      )}

      <button
        type="button"
        onClick={onSkip}
        className="mt-4 text-caption text-fg-subtle hover:text-fg-default hover:underline"
      >
        Skip — start blank
      </button>
    </div>
  );
}
