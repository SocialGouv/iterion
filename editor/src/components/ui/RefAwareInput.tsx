import { forwardRef, useCallback, useEffect, useImperativeHandle, useRef, useState } from "react";
import type { ChangeEvent, FocusEvent, KeyboardEvent } from "react";
import { Input, type InputProps } from "./Input";
import { Textarea, type TextareaProps } from "./Textarea";
import RefAwarePopup, { detectToken } from "./RefAwarePopup";
import type { RefContext } from "@/lib/refCompletion";

interface SharedRefAwareProps {
  value: string;
  onChange: (v: string) => void;
  refContext: RefContext;
}

/* -------------------------------------------------------------------------- */
/* Single-line input variant                                                  */
/* -------------------------------------------------------------------------- */

export type RefAwareInputProps = Omit<InputProps, "value" | "onChange"> & SharedRefAwareProps;

export const RefAwareInput = forwardRef<HTMLInputElement, RefAwareInputProps>(
  function RefAwareInput({ value, onChange, refContext, onKeyDown, onBlur, ...rest }, ref) {
    const innerRef = useRef<HTMLInputElement | null>(null);
    useImperativeHandle(ref, () => innerRef.current as HTMLInputElement, []);
    const [caret, setCaret] = useState(0);
    const [open, setOpen] = useState(false);

    const updateCaret = useCallback(() => {
      const el = innerRef.current;
      if (!el) return;
      setCaret(el.selectionStart ?? el.value.length);
    }, []);

    useEffect(() => {
      const el = innerRef.current;
      if (!el) return;
      // Recompute on selection changes inside the element (clicks, keyboard).
      const handler = () => updateCaret();
      el.addEventListener("select", handler);
      el.addEventListener("click", handler);
      el.addEventListener("keyup", handler);
      return () => {
        el.removeEventListener("select", handler);
        el.removeEventListener("click", handler);
        el.removeEventListener("keyup", handler);
      };
    }, [updateCaret]);

    useEffect(() => {
      // Open the popup when the caret sits inside an active `{{...` token.
      if (!innerRef.current) return;
      const tok = detectToken(value, caret);
      setOpen(!!tok);
    }, [value, caret]);

    const handleChange = (e: ChangeEvent<HTMLInputElement>) => {
      onChange(e.target.value);
      // Caret position is updated by the keyup event afterwards.
    };

    const handleKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
      onKeyDown?.(e);
    };

    const handleBlur = (e: FocusEvent<HTMLInputElement>) => {
      // Defer close so a click on the popup can fire first.
      window.setTimeout(() => setOpen(false), 100);
      onBlur?.(e);
    };

    return (
      <>
        <Input
          ref={innerRef}
          value={value}
          onChange={handleChange}
          onKeyDown={handleKeyDown}
          onBlur={handleBlur}
          {...rest}
        />
        {open && (
          <RefAwarePopup
            element={innerRef.current}
            value={value}
            caret={caret}
            refContext={refContext}
            onSelect={(next, nextCaret) => {
              onChange(next);
              window.setTimeout(() => {
                const el = innerRef.current;
                if (el) {
                  el.focus();
                  el.setSelectionRange(nextCaret, nextCaret);
                  setCaret(nextCaret);
                }
              }, 0);
              setOpen(false);
            }}
            onClose={() => setOpen(false)}
          />
        )}
      </>
    );
  },
);

/* -------------------------------------------------------------------------- */
/* Multi-line textarea variant                                                */
/* -------------------------------------------------------------------------- */

export type RefAwareTextareaProps = Omit<TextareaProps, "value" | "onChange"> & SharedRefAwareProps;

export const RefAwareTextarea = forwardRef<HTMLTextAreaElement, RefAwareTextareaProps>(
  function RefAwareTextarea({ value, onChange, refContext, onKeyDown, onBlur, ...rest }, ref) {
    const innerRef = useRef<HTMLTextAreaElement | null>(null);
    useImperativeHandle(ref, () => innerRef.current as HTMLTextAreaElement, []);
    const [caret, setCaret] = useState(0);
    const [open, setOpen] = useState(false);

    const updateCaret = useCallback(() => {
      const el = innerRef.current;
      if (!el) return;
      setCaret(el.selectionStart ?? el.value.length);
    }, []);

    useEffect(() => {
      const el = innerRef.current;
      if (!el) return;
      const handler = () => updateCaret();
      el.addEventListener("select", handler);
      el.addEventListener("click", handler);
      el.addEventListener("keyup", handler);
      return () => {
        el.removeEventListener("select", handler);
        el.removeEventListener("click", handler);
        el.removeEventListener("keyup", handler);
      };
    }, [updateCaret]);

    useEffect(() => {
      if (!innerRef.current) return;
      const tok = detectToken(value, caret);
      setOpen(!!tok);
    }, [value, caret]);

    const handleChange = (e: ChangeEvent<HTMLTextAreaElement>) => {
      onChange(e.target.value);
    };

    const handleBlur = (e: FocusEvent<HTMLTextAreaElement>) => {
      window.setTimeout(() => setOpen(false), 100);
      onBlur?.(e);
    };

    return (
      <>
        <Textarea
          ref={innerRef}
          value={value}
          onChange={handleChange}
          onKeyDown={onKeyDown}
          onBlur={handleBlur}
          {...rest}
        />
        {open && (
          <RefAwarePopup
            element={innerRef.current}
            value={value}
            caret={caret}
            refContext={refContext}
            onSelect={(next, nextCaret) => {
              onChange(next);
              window.setTimeout(() => {
                const el = innerRef.current;
                if (el) {
                  el.focus();
                  el.setSelectionRange(nextCaret, nextCaret);
                  setCaret(nextCaret);
                }
              }, 0);
              setOpen(false);
            }}
            onClose={() => setOpen(false)}
          />
        )}
      </>
    );
  },
);
