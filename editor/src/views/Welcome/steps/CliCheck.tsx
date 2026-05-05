import { useEffect, useState } from "react";

import { Badge, Button } from "@/components/ui";

import { desktop, type CLIStatus } from "@/lib/desktopBridge";

interface Props {
  onNext: () => void;
  onBack: () => void;
}

export default function CliCheck({ onNext, onBack }: Props) {
  const [statuses, setStatuses] = useState<CLIStatus[] | null>(null);

  useEffect(() => {
    desktop
      .detectExternalCLIs(false)
      .then(setStatuses)
      .catch((err) => {
        console.error(err);
        setStatuses([]);
      });
  }, []);

  return (
    <div className="max-w-2xl flex flex-col gap-4">
      <h2 className="text-lg font-semibold">Detected tools</h2>
      <p className="text-fg-subtle text-sm">
        These external CLIs unlock additional workflow backends. They are{" "}
        <strong>optional</strong> — Iterion's built-in claw backend works
        without any of them.
      </p>
      {statuses === null ? (
        <p>Detecting…</p>
      ) : (
        <ul className="flex flex-col">
          {statuses.map((s) => (
            <li
              key={s.name}
              className="flex items-center gap-3 border-b border-border-default py-2"
            >
              <div className="w-32 shrink-0 font-semibold">{s.name}</div>
              <div className="flex-1 min-w-0">
                {s.found ? (
                  <span className="text-sm">
                    <Badge variant="success">found</Badge>{" "}
                    <span className="text-fg-subtle truncate ml-1">
                      {s.path}
                      {s.version ? ` (${s.version})` : ""}
                    </span>
                  </span>
                ) : (
                  <Badge variant="warning">not found</Badge>
                )}
              </div>
              {!s.found && (
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => desktop.openExternal(s.install_url)}
                >
                  Install…
                </Button>
              )}
            </li>
          ))}
        </ul>
      )}
      <div className="flex gap-3">
        <Button onClick={onBack}>Back</Button>
        <Button onClick={onNext} variant="primary">
          Continue
        </Button>
      </div>
    </div>
  );
}
