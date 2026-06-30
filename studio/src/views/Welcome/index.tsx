import { useEffect, useRef, useState } from "react";

import { desktop } from "@/lib/desktopBridge";
import { useUIStore } from "@/store/ui";
import { toastError } from "@/lib/errorHints";

import ProjectPicker from "./steps/ProjectPicker";
import ApiKeys from "./steps/ApiKeys";
import CliCheck from "./steps/CliCheck";
import Done from "./steps/Done";

type Step = "project" | "api-keys" | "cli-check" | "done";

const stepOrder: Step[] = ["project", "api-keys", "cli-check", "done"];

const stepLabels: Record<Step, string> = {
  project: "Project",
  "api-keys": "API keys",
  "cli-check": "Tools",
  done: "Done",
};

interface WelcomeProps {
  onComplete: () => void;
}

export default function Welcome({ onComplete }: WelcomeProps) {
  const [step, setStep] = useState<Step>("project");
  const idx = stepOrder.indexOf(step);
  const mainRef = useRef<HTMLElement>(null);
  const didMount = useRef(false);

  // Move focus to the step region when the step changes so keyboard and
  // screen-reader users land on the new content (and the region's
  // aria-label announces "Step N of M"). Skip the initial mount so we
  // don't steal focus from a step's own autofocus.
  useEffect(() => {
    if (!didMount.current) {
      didMount.current = true;
      return;
    }
    mainRef.current?.focus();
  }, [step]);

  const goNext = () => {
    const next = stepOrder[idx + 1];
    if (next) setStep(next);
  };
  const goBack = () => {
    const prev = stepOrder[idx - 1];
    if (prev) setStep(prev);
  };
  const finish = async () => {
    try {
      await desktop.markFirstRunDone();
    } catch (err) {
      // Don't trap the user on the Done step — proceed into the app, but
      // surface the failure (previously a silent console.error) so they
      // know onboarding may re-appear next launch.
      console.error("Welcome: markFirstRunDone failed", err);
      toastError(useUIStore.getState().addToast, err, "Couldn't finish onboarding");
    }
    onComplete();
  };

  return (
    <div className="flex flex-col h-screen p-8 bg-surface-0 text-fg-default">
      <header className="mb-6">
        <h1 className="text-headline font-semibold">Welcome to Iterion</h1>
        <ol
          className="flex gap-3 mt-3 text-xs text-fg-subtle list-none p-0"
          aria-label="Onboarding progress"
        >
          {stepOrder.map((s, i) => (
            <li
              key={s}
              aria-current={s === step ? "step" : undefined}
              className={`${s === step ? "font-semibold text-fg-default" : ""} ${i <= idx ? "" : "opacity-40"}`}
            >
              {i + 1}. {stepLabels[s]}
              {i < idx && <span className="sr-only"> (completed)</span>}
            </li>
          ))}
        </ol>
      </header>
      <main
        ref={mainRef}
        tabIndex={-1}
        role="region"
        aria-label={`Step ${idx + 1} of ${stepOrder.length}: ${stepLabels[step]}`}
        className="flex-1 overflow-y-auto outline-none"
      >
        {step === "project" && <ProjectPicker onNext={goNext} />}
        {step === "api-keys" && <ApiKeys onNext={goNext} onBack={goBack} />}
        {step === "cli-check" && <CliCheck onNext={goNext} onBack={goBack} />}
        {step === "done" && <Done onFinish={finish} onBack={goBack} />}
      </main>
    </div>
  );
}
