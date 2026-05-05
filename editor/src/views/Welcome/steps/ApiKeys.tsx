import { Button } from "@/components/ui";

import ApiKeysEditor from "@/views/Settings/ApiKeysEditor";

interface Props {
  onNext: () => void;
  onBack: () => void;
}

export default function ApiKeys({ onNext, onBack }: Props) {
  return (
    <div className="max-w-2xl flex flex-col gap-4">
      <h2 className="text-lg font-semibold">Add your API keys (optional)</h2>
      <p className="text-fg-subtle text-sm">
        Keys are stored in your OS keychain (Keychain on macOS, Credential
        Manager on Windows, libsecret on Linux). Existing shell environment
        variables of the same name take precedence.
      </p>
      <ApiKeysEditor />
      <div className="flex gap-3">
        <Button onClick={onBack}>Back</Button>
        <Button onClick={onNext} variant="ghost">
          I'll add them later
        </Button>
        <Button onClick={onNext} variant="primary">
          Continue
        </Button>
      </div>
    </div>
  );
}
