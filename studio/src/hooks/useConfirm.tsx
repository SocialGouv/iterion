import { useCallback, useRef, useState, type ReactNode } from "react";
import ConfirmDialog from "@/components/shared/ConfirmDialog";

export interface ConfirmOptions {
  title: string;
  message: ReactNode;
  confirmLabel?: string;
  confirmVariant?: "default" | "danger";
}

// Confirmer is the ask-half of useConfirm, named so a component can take
// it as a prop without importing the hook's whole result shape.
export type Confirmer = (options: ConfirmOptions) => Promise<boolean>;

interface UseConfirmResult {
  confirm: Confirmer;
  /** Take down a pending question, settling its caller `false`. */
  dismiss: () => void;
  dialog: ReactNode;
}

// Promise-based wrapper around ConfirmDialog so call-sites keep an
// almost-synchronous shape:
//
//   const { confirm, dialog } = useConfirm();
//   const handleX = async () => {
//     if (isDirty() && !(await confirm({ title, message }))) return;
//     ...
//   };
//   return <>...{dialog}</>;
//
// One outstanding dialog per hook instance is enough — callers that
// need multiple concurrent confirms can mount the hook twice.
export function useConfirm(): UseConfirmResult {
  const [opts, setOpts] = useState<ConfirmOptions | null>(null);
  const resolverRef = useRef<((value: boolean) => void) | null>(null);

  const confirm = useCallback((options: ConfirmOptions) => {
    return new Promise<boolean>((resolve) => {
      // A question replaced is a question answered: this hook holds ONE
      // resolver slot, so a second `confirm()` used to strand the first
      // caller's promise for ever. `false` means ABANDON THE ACTION, which
      // is the safe default for every caller today (~50 sites: discards,
      // but also Merge, Approve, Rotate, Grant owner — abandoning each is
      // safe). A caller for which abandoning is NOT safe must not share a
      // hook instance.
      resolverRef.current?.(false);
      resolverRef.current = resolve;
      setOpts(options);
    });
  }, []);

  const settle = useCallback((value: boolean) => {
    const resolve = resolverRef.current;
    resolverRef.current = null;
    setOpts(null);
    resolve?.(value);
  }, []);

  /** Take down a question the asker can no longer answer, settling its
   *  caller. An effect that opened a confirm and is then torn down must call
   *  this, or a blocking modal outlives the question it was asking. */
  const dismiss = useCallback(() => {
    if (resolverRef.current) settle(false);
  }, [settle]);

  const dialog = opts ? (
    <ConfirmDialog
      open
      title={opts.title}
      message={opts.message}
      confirmLabel={opts.confirmLabel}
      confirmVariant={opts.confirmVariant}
      onConfirm={() => settle(true)}
      onCancel={() => settle(false)}
    />
  ) : null;

  return { confirm, dismiss, dialog };
}
