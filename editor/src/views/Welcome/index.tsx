import { useState } from "react";

import { desktop } from "@/lib/desktopBridge";

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
      console.error("Welcome: markFirstRunDone failed", err);
    }
    onComplete();
  };

  return (
    <div className="flex flex-col h-screen p-8 bg-surface-0 text-fg-default">
      <header className="mb-6">
        <h1 className="text-2xl font-semibold">Welcome to Iterion</h1>
        <ol className="flex gap-3 mt-3 text-xs text-fg-subtle list-none p-0">
          {stepOrder.map((s, i) => (
            <li
              key={s}
              className={`${s === step ? "font-semibold text-fg-default" : ""} ${i <= idx ? "" : "opacity-40"}`}
            >
              {i + 1}. {stepLabels[s]}
            </li>
          ))}
        </ol>
      </header>
      <main className="flex-1 overflow-y-auto">
        {step === "project" && <ProjectPicker onNext={goNext} />}
        {step === "api-keys" && <ApiKeys onNext={goNext} onBack={goBack} />}
        {step === "cli-check" && <CliCheck onNext={goNext} onBack={goBack} />}
        {step === "done" && <Done onFinish={finish} onBack={goBack} />}
      </main>
    </div>
  );
}
