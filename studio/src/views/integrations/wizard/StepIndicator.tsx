import { CheckIcon } from "@radix-ui/react-icons";

// Shared wizard step-indicator pills — extracted from ConnectRepoWizard so
// every guided flow (connect repo, bind bot, …) renders the same progress
// strip: done steps get a green check, the active step is emphasized,
// upcoming steps are dimmed.

export interface WizardStepDef {
  id: string;
  label: string;
}

export function StepIndicator({
  steps,
  current,
  ariaLabel,
}: {
  steps: WizardStepDef[];
  /** The active step id — must be one of `steps`. */
  current: string;
  ariaLabel: string;
}) {
  const idx = steps.findIndex((s) => s.id === current);
  return (
    <ol
      className="flex flex-wrap items-center gap-2 text-xs text-fg-subtle"
      aria-label={ariaLabel}
    >
      {steps.map((s, i) => {
        const done = i < idx;
        const active = i === idx;
        return (
          <li
            key={s.id}
            aria-current={active ? "step" : undefined}
            className={[
              "inline-flex items-center gap-1.5",
              active ? "text-fg-default font-semibold" : "",
              !active && !done ? "opacity-60" : "",
            ]
              .join(" ")
              .trim()}
          >
            <span
              className={[
                "inline-flex h-5 w-5 items-center justify-center rounded-full border text-caption shrink-0",
                done
                  ? "bg-success-soft text-success-fg border-success/40"
                  : active
                    ? "bg-accent-soft text-accent-text border-accent/40"
                    : "bg-surface-2 text-fg-muted border-border-default",
              ].join(" ")}
              aria-hidden
            >
              {done ? <CheckIcon className="h-3 w-3" /> : i + 1}
            </span>
            <span>{s.label}</span>
            {i < steps.length - 1 && (
              <span aria-hidden className="text-fg-subtle">
                →
              </span>
            )}
          </li>
        );
      })}
    </ol>
  );
}
