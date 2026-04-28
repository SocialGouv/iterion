import { useCallback, useEffect, useRef, useState, type KeyboardEvent } from "react";

const labelClass = "block text-xs text-fg-subtle mb-1";
const inputClass = "w-full bg-surface-1 border border-border-strong rounded px-2 py-1 text-sm text-fg-default focus:border-accent focus:outline-none";
const selectClass = inputClass;

function FieldLabel({ label, help }: { label: string; help?: string }) {
  return (
    <label className={labelClass}>
      {label}
      {help && (
        <span className="text-fg-subtle hover:text-fg-muted cursor-help ml-1" title={help}>?</span>
      )}
    </label>
  );
}

interface TextFieldProps {
  label: string;
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
  multiline?: boolean;
  rows?: number;
  help?: string;
}

export function TextField({ label, value, onChange, placeholder, multiline, rows = 3, help }: TextFieldProps) {
  return (
    <div className="mb-2">
      <FieldLabel label={label} help={help} />
      {multiline ? (
        <textarea
          className={inputClass + " resize-y"}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          placeholder={placeholder}
          rows={rows}
        />
      ) : (
        <input
          className={inputClass}
          type="text"
          value={value}
          onChange={(e) => onChange(e.target.value)}
          placeholder={placeholder}
        />
      )}
    </div>
  );
}

interface CommittedTextFieldProps {
  label: string;
  value: string;
  onChange: (v: string) => void;
  onCommit?: (newValue: string) => void;
  validate?: (v: string) => string | null;
  placeholder?: string;
  help?: string;
}

/** TextField that only commits on blur or Enter, not on every keystroke. Used for name/rename fields. */
export function CommittedTextField({ label, value, onChange, onCommit, validate, placeholder, help }: CommittedTextFieldProps) {
  const [draft, setDraft] = useState(value);
  const [error, setError] = useState<string | null>(null);
  const focusedRef = useRef(false);

  // Sync draft from prop when not focused
  useEffect(() => {
    if (!focusedRef.current) {
      setDraft(value);
      setError(null);
    }
  }, [value]);

  const commit = useCallback(() => {
    const trimmed = draft.trim();
    if (trimmed === value) {
      setError(null);
      return;
    }
    if (validate) {
      const err = validate(trimmed);
      if (err) {
        setError(err);
        setDraft(value);
        return;
      }
    }
    setError(null);
    onChange(trimmed);
    onCommit?.(trimmed);
  }, [draft, value, validate, onChange, onCommit]);

  const handleBlur = useCallback(() => {
    focusedRef.current = false;
    commit();
  }, [commit]);

  const handleKeyDown = useCallback(
    (e: KeyboardEvent) => {
      if (e.key === "Enter") {
        e.preventDefault();
        (e.target as HTMLInputElement).blur();
      } else if (e.key === "Escape") {
        setDraft(value);
        setError(null);
        (e.target as HTMLInputElement).blur();
      }
    },
    [value],
  );

  const isDirty = draft.trim() !== value;

  return (
    <div className="mb-2">
      <FieldLabel label={label} help={help} />
      <div className="flex gap-1">
        <input
          className={`${inputClass} flex-1${error ? " ring-1 ring-red-500 border-danger" : ""}`}
          type="text"
          value={draft}
          onChange={(e) => { setDraft(e.target.value); setError(null); }}
          onFocus={() => { focusedRef.current = true; }}
          onBlur={handleBlur}
          onKeyDown={handleKeyDown}
          placeholder={placeholder}
          title={error ?? undefined}
        />
        {isDirty && (
          <button
            className="bg-accent hover:bg-accent text-fg-default text-xs px-1.5 rounded shrink-0"
            onMouseDown={(e) => {
              e.preventDefault(); // prevent blur before commit
              commit();
              (document.activeElement as HTMLInputElement)?.blur();
            }}
            title="Confirm"
          >
            &#x2713;
          </button>
        )}
      </div>
      {error && <p className="text-[10px] text-danger mt-0.5">{error}</p>}
    </div>
  );
}

interface NumberFieldProps {
  label: string;
  value: number | undefined;
  onChange: (v: number | undefined) => void;
  placeholder?: string;
  min?: number;
  help?: string;
}

export function NumberField({ label, value, onChange, placeholder, min, help }: NumberFieldProps) {
  return (
    <div className="mb-2">
      <FieldLabel label={label} help={help} />
      <input
        className={inputClass}
        type="number"
        value={value ?? ""}
        onChange={(e) => onChange(e.target.value === "" ? undefined : Number(e.target.value))}
        placeholder={placeholder}
        min={min}
      />
    </div>
  );
}

interface SelectFieldProps {
  label: string;
  value: string;
  onChange: (v: string) => void;
  options: { value: string; label: string }[];
  allowEmpty?: boolean;
  emptyLabel?: string;
  help?: string;
}

export function SelectField({ label, value, onChange, options, allowEmpty, emptyLabel = "-- none --", help }: SelectFieldProps) {
  return (
    <div className="mb-2">
      <FieldLabel label={label} help={help} />
      <select className={selectClass} value={value} onChange={(e) => onChange(e.target.value)}>
        {allowEmpty && <option value="">{emptyLabel}</option>}
        {options.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
    </div>
  );
}

interface SelectFieldWithCreateProps extends SelectFieldProps {
  onCreate: () => string; // returns the new name
}

export function SelectFieldWithCreate({ label, value, onChange, options, allowEmpty, emptyLabel, onCreate, help }: SelectFieldWithCreateProps) {
  return (
    <div className="mb-2">
      <FieldLabel label={label} help={help} />
      <div className="flex gap-1">
        <select className={selectClass + " flex-1"} value={value} onChange={(e) => onChange(e.target.value)}>
          {allowEmpty && <option value="">{emptyLabel ?? "-- none --"}</option>}
          {options.map((o) => (
            <option key={o.value} value={o.value}>
              {o.label}
            </option>
          ))}
        </select>
        <button
          className="bg-success hover:bg-success text-xs px-1.5 rounded shrink-0"
          onClick={() => {
            const newName = onCreate();
            onChange(newName);
          }}
          title={`Create new ${label.toLowerCase()}`}
        >
          +
        </button>
      </div>
    </div>
  );
}

interface CheckboxFieldProps {
  label: string;
  checked: boolean;
  onChange: (v: boolean) => void;
  help?: string;
}

export function CheckboxField({ label, checked, onChange, help }: CheckboxFieldProps) {
  return (
    <div className="mb-2 flex items-center gap-2">
      <input
        type="checkbox"
        checked={checked}
        onChange={(e) => onChange(e.target.checked)}
        className="rounded border-border-strong bg-surface-1"
      />
      <label className="text-xs text-fg-subtle">
        {label}
        {help && (
          <span className="text-fg-subtle hover:text-fg-muted cursor-help ml-1" title={help}>?</span>
        )}
      </label>
    </div>
  );
}

interface TagListFieldProps {
  label: string;
  values: string[];
  onChange: (v: string[]) => void;
  placeholder?: string;
}

export function TagListField({ label, values, onChange, placeholder = "Add..." }: TagListFieldProps) {
  const [input, setInput] = useState("");

  const addTag = useCallback(() => {
    const v = input.trim();
    if (v && !values.includes(v)) {
      onChange([...values, v]);
    }
    setInput("");
  }, [input, values, onChange]);

  const onKeyDown = useCallback(
    (e: KeyboardEvent) => {
      if (e.key === "Enter") {
        e.preventDefault();
        addTag();
      }
    },
    [addTag],
  );

  return (
    <div className="mb-2">
      <label className={labelClass}>{label}</label>
      <div className="flex flex-wrap gap-1 mb-1">
        {values.map((v) => (
          <span key={v} className="bg-surface-2 text-xs px-2 py-0.5 rounded flex items-center gap-1">
            {v}
            <button
              className="text-fg-subtle hover:text-fg-default"
              onClick={() => onChange(values.filter((x) => x !== v))}
            >
              x
            </button>
          </span>
        ))}
      </div>
      <div className="flex gap-1">
        <input
          className={inputClass}
          type="text"
          value={input}
          onChange={(e) => setInput(e.target.value)}
          onKeyDown={onKeyDown}
          placeholder={placeholder}
        />
        <button
          className="bg-surface-2 hover:bg-surface-3 text-xs px-2 rounded"
          onClick={addTag}
        >
          +
        </button>
      </div>
    </div>
  );
}

interface MultiSelectFieldProps {
  label: string;
  values: string[];
  onChange: (v: string[]) => void;
  options: string[];
}

export function MultiSelectField({ label, values, onChange, options }: MultiSelectFieldProps) {
  return (
    <div className="mb-2">
      <label className={labelClass}>{label}</label>
      <div className="flex flex-col gap-1 max-h-32 overflow-y-auto bg-surface-1 border border-border-strong rounded p-1">
        {options.map((opt) => (
          <label key={opt} className="flex items-center gap-2 text-xs text-fg-muted px-1 hover:bg-surface-2 rounded cursor-pointer">
            <input
              type="checkbox"
              checked={values.includes(opt)}
              onChange={(e) => {
                if (e.target.checked) {
                  onChange([...values, opt]);
                } else {
                  onChange(values.filter((v) => v !== opt));
                }
              }}
            />
            {opt}
          </label>
        ))}
        {options.length === 0 && <span className="text-xs text-fg-subtle px-1">No options available</span>}
      </div>
    </div>
  );
}
