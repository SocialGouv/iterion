import { useState } from "react";

import { isValidSecretName } from "@/api/secrets";
import { Input } from "@/components/ui/Input";

interface Props {
  value: string;
  onChange: (name: string) => void;
  placeholder: string;
}

/**
 * SecretNameField is the name input of an "add secret" dialog together with
 * its validation line.
 *
 * The verdict of isValidSecretName is displayed only once the user has typed
 * in the field or left it. A dialog that has just opened holds an empty name:
 * that is a field to fill in, not an error to report. The dialog's submit
 * button reads the same validator directly and stays disabled regardless of
 * what this line shows.
 */
export function SecretNameField({ value, onChange, placeholder }: Props) {
  const [interacted, setInteracted] = useState(false);
  const verdict = isValidSecretName(value);
  const showError = interacted && !verdict.ok;
  return (
    <label className="block">
      <div className="text-xs text-fg-muted mb-1">Name</div>
      <Input
        value={value}
        onChange={(e) => {
          setInteracted(true);
          onChange(e.target.value);
        }}
        onBlur={() => setInteracted(true)}
        error={showError}
        placeholder={placeholder}
        // A secret input follows this one in the dialog: without the hint a
        // browser reads the pair as a login form and offers the saved username
        // here.
        autoComplete="off"
        autoFocus
      />
      {/* The line keeps its height while it has nothing to say, so the dialog
          does not shift under the pointer when the verdict appears on blur. */}
      <div className={`text-xs mt-1 ${showError ? "text-danger" : "text-fg-muted"}`}>
        {verdict.ok ? "OK" : showError ? (verdict.error ?? "—") : "\u00a0"}
      </div>
    </label>
  );
}
