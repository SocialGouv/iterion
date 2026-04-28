import { forwardRef, type TextareaHTMLAttributes } from "react";

export interface TextareaProps extends TextareaHTMLAttributes<HTMLTextAreaElement> {
  error?: boolean;
}

export const Textarea = forwardRef<HTMLTextAreaElement, TextareaProps>(
  function Textarea({ className = "", error = false, ...rest }, ref) {
    const ringClass = error
      ? "border-danger focus:border-danger focus:ring-1 focus:ring-danger"
      : "border-border-strong focus:border-accent focus:ring-1 focus:ring-accent";
    const base =
      "w-full bg-surface-1 text-fg-default text-sm rounded-md border outline-none transition-colors px-2.5 py-1.5 placeholder:text-fg-subtle disabled:opacity-60 disabled:cursor-not-allowed resize-y";
    return (
      <textarea
        ref={ref}
        className={`${base} ${ringClass} ${className}`.trim()}
        {...rest}
      />
    );
  },
);
