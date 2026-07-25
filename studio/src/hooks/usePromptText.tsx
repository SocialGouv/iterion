import { useCallback, useRef, useState, type ReactNode } from "react";

import { Button, Dialog, Input } from "@/components/ui";

export interface PromptTextOptions {
  title: string;
  message?: ReactNode;
  label?: string;
  placeholder?: string;
  defaultValue?: string;
  confirmLabel?: string;
  /** Return an error string to keep the dialog open, or null when the value is valid. */
  validate?: (value: string) => string | null;
}

interface UsePromptTextResult {
  prompt: (options: PromptTextOptions) => Promise<string | null>;
  dialog: ReactNode;
}

// Promise-based single-line text prompt, the styled + accessible replacement for
// window.prompt() (banned by the a11y source-discipline test). Mirrors
// useConfirm(): call `prompt({...})`, await the entered string (or null on
// cancel), and render `dialog`.
//
//   const { prompt, dialog } = usePromptText();
//   const slug = await prompt({ title: "New bot id", validate: isValidSlug });
//   if (!slug) return;
//   return <>...{dialog}</>;
export function usePromptText(): UsePromptTextResult {
  const [opts, setOpts] = useState<PromptTextOptions | null>(null);
  const [value, setValue] = useState("");
  const [error, setError] = useState<string | null>(null);
  const resolverRef = useRef<((value: string | null) => void) | null>(null);

  const prompt = useCallback((options: PromptTextOptions) => {
    return new Promise<string | null>((resolve) => {
      resolverRef.current = resolve;
      setValue(options.defaultValue ?? "");
      setError(null);
      setOpts(options);
    });
  }, []);

  const settle = useCallback((result: string | null) => {
    const resolve = resolverRef.current;
    resolverRef.current = null;
    setOpts(null);
    setError(null);
    resolve?.(result);
  }, []);

  const submit = useCallback(() => {
    const v = value.trim();
    const err = opts?.validate?.(v) ?? (v === "" ? "Required" : null);
    if (err) {
      setError(err);
      return;
    }
    settle(v);
  }, [value, opts, settle]);

  const dialog = opts ? (
    <Dialog
      open
      onOpenChange={(o) => {
        if (!o) settle(null);
      }}
      title={opts.title}
      description={opts.message}
      stack="confirm"
      footer={
        <>
          <Button variant="ghost" size="sm" onClick={() => settle(null)}>
            Cancel
          </Button>
          <Button variant="primary" size="sm" onClick={submit}>
            {opts.confirmLabel ?? "OK"}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-1.5">
        {opts.label && <label className="text-caption text-fg-muted">{opts.label}</label>}
        <Input
          autoFocus
          value={value}
          placeholder={opts.placeholder}
          onChange={(e) => setValue(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") submit();
          }}
        />
        {error && <span className="text-caption text-danger">{error}</span>}
      </div>
    </Dialog>
  ) : null;

  return { prompt, dialog };
}
